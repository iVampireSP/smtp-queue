package pool

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ivampiresp/smtp-queue/config"
	"github.com/rs/zerolog/log"
)

// ErrPoolClosed 连接池已关闭时返回的错误
var ErrPoolClosed = errors.New("连接池已关闭")

// Conn 连接池中的一个 SMTP 连接
type Conn struct {
	Client     *smtp.Client
	rawConn    net.Conn
	serverName string
	createdAt  time.Time
	lastUsed   time.Time
	msgCount   int
}

// serverPool 单个 SMTP 服务器的连接池
type serverPool struct {
	mu     sync.Mutex
	server *config.SMTPServer
	idle   []*Conn
	active int
	sem    chan struct{} // 限制并发活跃连接数
}

// Pool 管理所有 SMTP 服务器的连接池
type Pool struct {
	mu       sync.RWMutex
	pools    map[string]*serverPool
	cfg      config.PoolConfig
	stopCh   chan struct{}
	stopped  sync.WaitGroup
	closed   int32          // atomic: 0=open, 1=closed
	activeWg sync.WaitGroup // 追踪所有活跃连接（Get 后未 Put/Discard 的）
}

// New 创建连接池
func New(cfg config.PoolConfig) *Pool {
	// 默认值
	if cfg.MaxIdlePerServer <= 0 {
		cfg.MaxIdlePerServer = 5
	}
	if cfg.MaxActivePerServer <= 0 {
		cfg.MaxActivePerServer = 10
	}
	if cfg.MaxIdleTimeSec <= 0 {
		cfg.MaxIdleTimeSec = 300
	}
	if cfg.MaxLifetimeSec <= 0 {
		cfg.MaxLifetimeSec = 1800
	}
	if cfg.MaxMessagesPerConn <= 0 {
		cfg.MaxMessagesPerConn = 100
	}
	if cfg.DialTimeoutSec <= 0 {
		cfg.DialTimeoutSec = 30
	}

	p := &Pool{
		pools:  make(map[string]*serverPool),
		cfg:    cfg,
		stopCh: make(chan struct{}),
	}

	// 后台清理 stale 连接
	p.stopped.Add(1)
	go p.cleaner()

	return p
}

// Close 关闭所有连接并停止后台清理
func (p *Pool) Close() {
	if !atomic.CompareAndSwapInt32(&p.closed, 0, 1) {
		return
	}

	close(p.stopCh)
	p.stopped.Wait()

	// 等待所有活跃连接归还
	p.activeWg.Wait()

	p.mu.Lock()
	defer p.mu.Unlock()

	for _, sp := range p.pools {
		sp.mu.Lock()
		for _, conn := range sp.idle {
			if err := conn.Client.Quit(); err != nil {
				log.Debug().Err(err).Str("server", conn.serverName).Msg("关闭空闲连接 Quit 失败")
			}
			conn.rawConn.Close()
		}
		sp.idle = nil
		sp.mu.Unlock()
	}
}

// Get 从连接池获取一个 SMTP 连接
func (p *Pool) Get(ctx context.Context, server *config.SMTPServer) (*Conn, error) {
	if atomic.LoadInt32(&p.closed) == 1 {
		return nil, ErrPoolClosed
	}

	sp := p.getOrCreateServerPool(server)

	// 限制每个服务器的最大活跃连接数（支持 context 取消）
	select {
	case sp.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	sp.mu.Lock()

	// 先收集所有过期连接，在锁外统一关闭
	now := time.Now()
	var stale []*Conn
	valid := make([]*Conn, 0, len(sp.idle))
	for _, conn := range sp.idle {
		if p.isExpired(conn, now) {
			stale = append(stale, conn)
		} else {
			valid = append(valid, conn)
		}
	}
	sp.idle = valid

	// 尝试从空闲池获取（LIFO：最近使用的优先）
	var candidate *Conn
	if len(sp.idle) > 0 {
		n := len(sp.idle) - 1
		candidate = sp.idle[n]
		sp.idle[n] = nil // 帮助 GC
		sp.idle = sp.idle[:n]
	}

	sp.active++
	sp.mu.Unlock()

	// 锁外关闭过期连接
	for _, c := range stale {
		if err := c.Client.Quit(); err != nil {
			log.Debug().Err(err).Str("server", c.serverName).Msg("清理过期连接 Quit 失败")
		}
		c.rawConn.Close()
	}

	if candidate != nil {
		// 健康检查：NOOP（带 5 秒超时）
		candidate.rawConn.SetDeadline(time.Now().Add(5 * time.Second))
		err := candidate.Client.Noop()
		candidate.rawConn.SetDeadline(time.Time{}) // 清除 deadline

		if err != nil {
			candidate.rawConn.Close()
			// candidate 已坏，dial 新连接
			// 注意：active 已经 ++，dial 失败时会负责 active-- 和释放信号量
			conn, dialErr := p.dial(server, sp)
			if dialErr != nil {
				return nil, dialErr
			}
			p.activeWg.Add(1)
			return conn, nil
		}

		candidate.lastUsed = now
		p.activeWg.Add(1)
		return candidate, nil
	}

	// 没有空闲连接，创建新的
	conn, err := p.dial(server, sp)
	if err != nil {
		return nil, err
	}
	p.activeWg.Add(1)
	return conn, nil
}

// Put 将连接归还到池中
func (p *Pool) Put(conn *Conn) {
	defer p.activeWg.Done()

	sp := p.findServerPool(conn.serverName)
	if sp == nil {
		log.Error().Str("server", conn.serverName).Msg("Put: 未找到 serverPool，连接将被关闭")
		if err := conn.Client.Quit(); err != nil {
			log.Debug().Err(err).Msg("Put: Quit 失败")
		}
		conn.rawConn.Close()
		return
	}

	conn.msgCount++
	conn.lastUsed = time.Now()

	// 连接池已关闭、连接过期或超过单连接最大消息数，直接丢弃
	if atomic.LoadInt32(&p.closed) == 1 ||
		conn.msgCount >= p.cfg.MaxMessagesPerConn ||
		p.isExpired(conn, conn.lastUsed) {
		p.doDiscard(conn, sp)
		return
	}

	sp.mu.Lock()
	if len(sp.idle) >= p.cfg.MaxIdlePerServer {
		// 空闲池已满，关闭连接
		sp.active--
		sp.mu.Unlock()
		<-sp.sem
		if err := conn.Client.Quit(); err != nil {
			log.Debug().Err(err).Msg("Put: 空闲池满 Quit 失败")
		}
		conn.rawConn.Close()
		return
	}
	sp.idle = append(sp.idle, conn)
	sp.active--
	sp.mu.Unlock()
	<-sp.sem
}

// Discard 丢弃一个坏连接
func (p *Pool) Discard(conn *Conn) {
	defer p.activeWg.Done()

	sp := p.findServerPool(conn.serverName)
	if sp == nil {
		log.Error().Str("server", conn.serverName).Msg("Discard: 未找到 serverPool，连接将被关闭")
		conn.rawConn.Close()
		return
	}
	p.doDiscard(conn, sp)
}

func (p *Pool) doDiscard(conn *Conn, sp *serverPool) {
	if err := conn.Client.Quit(); err != nil {
		log.Debug().Err(err).Str("server", conn.serverName).Msg("Discard: Quit 失败")
	}
	conn.rawConn.Close()

	sp.mu.Lock()
	sp.active--
	sp.mu.Unlock()
	<-sp.sem
}

// getOrCreateServerPool 获取或创建某个 SMTP 服务器的连接池
func (p *Pool) getOrCreateServerPool(server *config.SMTPServer) *serverPool {
	p.mu.RLock()
	sp, ok := p.pools[server.Name]
	p.mu.RUnlock()

	if ok {
		return sp
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// double check
	if sp, ok = p.pools[server.Name]; ok {
		return sp
	}

	sp = &serverPool{
		server: server,
		sem:    make(chan struct{}, p.cfg.MaxActivePerServer),
	}
	p.pools[server.Name] = sp
	return sp
}

// findServerPool 通过 name 获取已存在的 serverPool
func (p *Pool) findServerPool(name string) *serverPool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pools[name]
}

// isExpired 检查连接是否已过期
func (p *Pool) isExpired(conn *Conn, now time.Time) bool {
	maxIdle := time.Duration(p.cfg.MaxIdleTimeSec) * time.Second
	maxLife := time.Duration(p.cfg.MaxLifetimeSec) * time.Second

	if now.Sub(conn.lastUsed) > maxIdle {
		return true
	}
	if now.Sub(conn.createdAt) > maxLife {
		return true
	}
	return false
}

// dial 创建新的 SMTP 连接
func (p *Pool) dial(server *config.SMTPServer, sp *serverPool) (*Conn, error) {
	addr := net.JoinHostPort(server.Host, fmt.Sprintf("%d", server.Port))
	host, _, _ := net.SplitHostPort(addr)
	timeout := time.Duration(p.cfg.DialTimeoutSec) * time.Second

	var rawConn net.Conn
	var err error

	switch server.Encryption {
	case "ssl":
		dialer := &net.Dialer{Timeout: timeout}
		rawConn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: host})
	default:
		rawConn, err = net.DialTimeout("tcp", addr, timeout)
	}

	if err != nil {
		sp.mu.Lock()
		sp.active--
		sp.mu.Unlock()
		<-sp.sem
		return nil, fmt.Errorf("连接 SMTP 服务器 %s: %w", server.Name, err)
	}

	client, err := smtp.NewClient(rawConn, host)
	if err != nil {
		rawConn.Close()
		sp.mu.Lock()
		sp.active--
		sp.mu.Unlock()
		<-sp.sem
		return nil, fmt.Errorf("创建 SMTP 客户端 %s: %w", server.Name, err)
	}

	// STARTTLS
	if server.Encryption == "tls" {
		if err = client.StartTLS(&tls.Config{ServerName: host}); err != nil {
			rawConn.Close()
			sp.mu.Lock()
			sp.active--
			sp.mu.Unlock()
			<-sp.sem
			return nil, fmt.Errorf("STARTTLS %s: %w", server.Name, err)
		}
	}

	// 认证
	if server.Username != "" && server.Password != "" {
		auth := smtp.PlainAuth("", server.Username, server.Password, host)
		if err = client.Auth(auth); err != nil {
			rawConn.Close()
			sp.mu.Lock()
			sp.active--
			sp.mu.Unlock()
			<-sp.sem
			return nil, fmt.Errorf("SMTP 认证 %s: %w", server.Name, err)
		}
	}

	now := time.Now()
	return &Conn{
		Client:     client,
		rawConn:    rawConn,
		serverName: server.Name,
		createdAt:  now,
		lastUsed:   now,
		msgCount:   0,
	}, nil
}

// cleaner 后台定时清理 stale 空闲连接
func (p *Pool) cleaner() {
	defer p.stopped.Done()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.cleanStale()
		}
	}
}

func (p *Pool) cleanStale() {
	p.mu.RLock()
	pools := make([]*serverPool, 0, len(p.pools))
	for _, sp := range p.pools {
		pools = append(pools, sp)
	}
	p.mu.RUnlock()

	now := time.Now()
	totalCleaned := 0

	for _, sp := range pools {
		sp.mu.Lock()
		alive := make([]*Conn, 0, len(sp.idle))
		var stale []*Conn

		for _, conn := range sp.idle {
			if p.isExpired(conn, now) {
				stale = append(stale, conn)
			} else {
				alive = append(alive, conn)
			}
		}
		sp.idle = alive
		sp.mu.Unlock()

		for _, conn := range stale {
			if err := conn.Client.Quit(); err != nil {
				log.Debug().Err(err).Str("server", conn.serverName).Msg("清理: Quit 失败")
			}
			conn.rawConn.Close()
			totalCleaned++
		}
	}

	if totalCleaned > 0 {
		log.Debug().Int("count", totalCleaned).Msg("清理过期 SMTP 空闲连接")
	}
}
