package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/ivampiresp/smtp-queue/config"
	"github.com/ivampiresp/smtp-queue/queue"
	"github.com/rs/zerolog/log"
)

// 服务器状态码
const (
	statusReadyPrefix       = "220 "
	statusOK                = "250 OK"
	statusStartMail         = "250 Go ahead"
	statusDataReady         = "354 Start mail input; end with <CRLF>.<CRLF>"
	statusClosing           = "221 Bye"
	statusCommandUnknown    = "500 Command unrecognized"
	statusSyntaxError       = "501 Syntax error"
	statusCommandNotImplErr = "502 Command not implemented"
	statusBadSequence       = "503 Bad sequence of commands"
)

// 限制常量
const (
	maxConnections = 100             // 最大并发连接数
	maxDataSize    = 10 << 20        // 最大邮件数据 10MB
	maxRecipients  = 100             // 单封邮件最大收件人数
	idleTimeout    = 5 * time.Minute // 空闲超时
)

// Server 是一个简单的SMTP服务器，它接收邮件并将其添加到发送队列中
type Server struct {
	Queue  *queue.Queue
	Config *config.Config

	mu       sync.Mutex
	listener net.Listener
	connWg   sync.WaitGroup
	connSem  chan struct{}
}

// New 创建新的SMTP服务器实例
func New(q *queue.Queue, cfg *config.Config) *Server {
	return &Server{
		Queue:   q,
		Config:  cfg,
		connSem: make(chan struct{}, maxConnections),
	}
}

// Start 启动SMTP服务器
func (s *Server) Start() error {
	var err error
	listener, err := net.Listen("tcp", s.Config.ListenAddr)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.listener = listener
	s.mu.Unlock()

	log.Info().Str("addr", s.Config.ListenAddr).Msg("SMTP服务器开始监听")

	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			log.Error().Err(err).Msg("接受连接时出错")
			continue
		}

		// 并发连接数限制
		select {
		case s.connSem <- struct{}{}:
		default:
			log.Warn().Str("client", conn.RemoteAddr().String()).Msg("连接数已满，拒绝连接")
			conn.Close()
			continue
		}

		s.connWg.Add(1)
		go func() {
			defer s.connWg.Done()
			defer func() { <-s.connSem }()
			s.handleConnection(conn)
		}()
	}
}

// Stop 停止SMTP服务器并等待活跃连接排空
func (s *Server) Stop() error {
	s.mu.Lock()
	listener := s.listener
	s.mu.Unlock()

	if listener != nil {
		if err := listener.Close(); err != nil {
			return err
		}
	}

	// 等待活跃连接结束（带超时）
	done := make(chan struct{})
	go func() {
		s.connWg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		log.Warn().Msg("等待活跃 SMTP 连接排空超时")
	}

	return nil
}

// 处理客户端连接
func (s *Server) handleConnection(conn net.Conn) {
	defer conn.Close()

	client := conn.RemoteAddr().String()

	// 创建会话
	session := newSession(conn, s.Queue, s.Config)

	// 发送欢迎消息
	session.send(statusReadyPrefix + s.Config.Banner)

	// 处理会话，每次命令后重置空闲超时
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20) // 最大行 1MB（base64 附件）

	hasCommand := false
	conn.SetDeadline(time.Now().Add(idleTimeout))
	for scanner.Scan() {
		// 首条命令时记录连接（过滤健康检查等无交互连接）
		if !hasCommand {
			hasCommand = true
			log.Info().Str("client", client).Msg("客户端连接")
		}

		// 每次收到数据后重置空闲超时
		conn.SetDeadline(time.Now().Add(idleTimeout))

		if err := session.handleCommand(scanner.Text()); err != nil {
			log.Error().Err(err).Msg("处理命令时出错")
			break
		}

		if session.quit {
			break
		}
	}

	if err := scanner.Err(); err != nil {
		log.Error().Err(err).Msg("读取客户端数据时出错")
	}

	if hasCommand {
		log.Info().Str("client", client).Msg("客户端断开连接")
	}
}

// smtpSession 表示一个SMTP会话
type smtpSession struct {
	conn  net.Conn
	queue *queue.Queue
	cfg   *config.Config

	// 会话状态
	helo     string
	mailFrom string
	rcptTo   []string
	data     []string
	dataSize int
	inData   bool
	quit     bool
}

// 创建新的SMTP会话
func newSession(conn net.Conn, q *queue.Queue, cfg *config.Config) *smtpSession {
	return &smtpSession{
		conn:  conn,
		queue: q,
		cfg:   cfg,
	}
}

// 发送响应到客户端
func (s *smtpSession) send(message string) {
	fmt.Fprintf(s.conn, "%s\r\n", message)
}

// 处理SMTP命令
func (s *smtpSession) handleCommand(line string) error {
	if s.inData {
		return s.handleData(line)
	}

	// 拆分命令和参数
	parts := strings.SplitN(line, " ", 2)
	command := strings.ToUpper(parts[0])
	args := ""
	if len(parts) > 1 {
		args = parts[1]
	}

	switch command {
	case "HELO", "EHLO":
		return s.handleHelo(args)
	case "MAIL":
		return s.handleMail(args)
	case "RCPT":
		return s.handleRcpt(args)
	case "DATA":
		return s.handleDataCommand()
	case "RSET":
		return s.handleRset()
	case "NOOP":
		s.send(statusOK)
	case "QUIT":
		return s.handleQuit()
	default:
		s.send(statusCommandUnknown)
	}

	return nil
}

// 处理HELO/EHLO命令
func (s *smtpSession) handleHelo(args string) error {
	if args == "" {
		s.send(statusSyntaxError)
		return nil
	}

	s.helo = args
	s.send(statusOK)
	return nil
}

// extractAddress 从 SMTP 参数中提取邮件地址
func extractAddress(raw string) string {
	addr := strings.TrimSpace(raw)
	addr = strings.TrimPrefix(addr, "<")
	addr = strings.TrimSuffix(addr, ">")
	addr = strings.TrimSpace(addr)
	return strings.ToLower(addr)
}

// 处理MAIL FROM命令
func (s *smtpSession) handleMail(args string) error {
	if s.helo == "" {
		s.send(statusBadSequence)
		return nil
	}

	if !strings.HasPrefix(strings.ToUpper(args), "FROM:") {
		s.send(statusSyntaxError)
		return nil
	}

	mailFrom := extractAddress(args[5:])
	if mailFrom == "" {
		s.send(statusSyntaxError)
		return nil
	}

	s.mailFrom = mailFrom
	s.send(statusStartMail)
	return nil
}

// 处理RCPT TO命令
func (s *smtpSession) handleRcpt(args string) error {
	if s.mailFrom == "" {
		s.send(statusBadSequence)
		return nil
	}

	if !strings.HasPrefix(strings.ToUpper(args), "TO:") {
		s.send(statusSyntaxError)
		return nil
	}

	if len(s.rcptTo) >= maxRecipients {
		s.send("452 Too many recipients")
		return nil
	}

	rcptTo := extractAddress(args[3:])
	if rcptTo == "" {
		s.send(statusSyntaxError)
		return nil
	}

	s.rcptTo = append(s.rcptTo, rcptTo)
	s.send(statusOK)
	return nil
}

// 处理DATA命令
func (s *smtpSession) handleDataCommand() error {
	if len(s.rcptTo) == 0 {
		s.send(statusBadSequence)
		return nil
	}

	s.inData = true
	s.dataSize = 0
	s.send(statusDataReady)
	return nil
}

// 处理DATA内容
func (s *smtpSession) handleData(line string) error {
	// 数据结束标记
	if line == "." {
		s.inData = false

		if err := s.processEmail(); err != nil {
			log.Error().Err(err).Msg("处理邮件时出错")
			s.send("554 Transaction failed")
			return nil
		}

		s.send(statusOK)
		return nil
	}

	// 处理行首的点
	if strings.HasPrefix(line, ".") {
		line = line[1:]
	}

	// DATA 大小限制
	s.dataSize += len(line) + 2 // +2 for \r\n
	if s.dataSize > maxDataSize {
		s.inData = false
		s.data = nil
		s.dataSize = 0
		s.send("552 Too much mail data")
		return nil
	}

	s.data = append(s.data, line)
	return nil
}

// 处理RSET命令
func (s *smtpSession) handleRset() error {
	s.mailFrom = ""
	s.rcptTo = nil
	s.data = nil
	s.dataSize = 0
	s.inData = false

	s.send(statusOK)
	return nil
}

// 处理QUIT命令
func (s *smtpSession) handleQuit() error {
	s.send(statusClosing)
	s.quit = true
	return nil
}

// 处理接收到的邮件
func (s *smtpSession) processEmail() error {
	if len(s.data) == 0 {
		return errors.New("邮件内容为空")
	}

	// 解析邮件内容以获取主题（用于日志记录）
	var subject string
	for _, line := range s.data {
		if strings.HasPrefix(strings.ToLower(line), "subject:") {
			subject = strings.TrimSpace(line[8:])
			break
		}
	}

	if subject == "" {
		subject = "(无主题)"
	}

	clientFrom := s.mailFrom

	// 保留原始邮件内容，包括所有邮件头和正文
	originalContent := strings.Join(s.data, "\r\n")

	_, err := s.queue.Enqueue(context.Background(), clientFrom, s.rcptTo, subject, originalContent)
	if err != nil {
		return fmt.Errorf("将邮件添加到队列时出错: %w", err)
	}

	log.Info().
		Str("from", clientFrom).
		Strs("to", s.rcptTo).
		Str("subject", subject).
		Msg("邮件已入队")

	// 重置会话状态
	s.mailFrom = ""
	s.rcptTo = nil
	s.data = nil
	s.dataSize = 0

	return nil
}
