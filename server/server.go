package server

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/ivampiresp/smtp-queue/config"
	"github.com/ivampiresp/smtp-queue/db"
	"github.com/rs/zerolog/log"
)

// 服务器状态码
const (
	statusReady             = "220 SMTP Queue Server Ready"
	statusOK                = "250 OK"
	statusStartMail         = "250 Go ahead"
	statusDataReady         = "354 Start mail input; end with <CRLF>.<CRLF>"
	statusClosing           = "221 Bye"
	statusCommandUnknown    = "500 Command unrecognized"
	statusSyntaxError       = "501 Syntax error"
	statusCommandNotImplErr = "502 Command not implemented"
	statusBadSequence       = "503 Bad sequence of commands"
)

// Server 是一个简单的SMTP服务器，它接收邮件并将其添加到发送队列中
type Server struct {
	DB     *db.DB
	Config *config.Config

	listener net.Listener
}

// New 创建新的SMTP服务器实例
func New(database *db.DB, cfg *config.Config) *Server {
	return &Server{
		DB:     database,
		Config: cfg,
	}
}

// Start 启动SMTP服务器
func (s *Server) Start() error {
	var err error
	s.listener, err = net.Listen("tcp", s.Config.ListenAddr)
	if err != nil {
		return err
	}

	log.Info().Str("addr", s.Config.ListenAddr).Msg("SMTP服务器开始监听")

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			log.Error().Err(err).Msg("接受连接时出错")
			continue
		}

		go s.handleConnection(conn)
	}
}

// Stop 停止SMTP服务器
func (s *Server) Stop() error {
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

// 处理客户端连接
func (s *Server) handleConnection(conn net.Conn) {
	defer conn.Close()

	log.Info().Str("client", conn.RemoteAddr().String()).Msg("客户端连接")

	// 设置连接超时
	conn.SetDeadline(time.Now().Add(5 * time.Minute))

	// 创建会话
	session := newSession(conn, s.DB, s.Config)

	// 发送欢迎消息
	session.send(statusReady)

	// 处理会话
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		if err := session.handleCommand(scanner.Text()); err != nil {
			log.Error().Err(err).Msg("处理命令时出错")
			break
		}

		// 如果会话已关闭则退出
		if session.quit {
			break
		}
	}

	if err := scanner.Err(); err != nil {
		log.Error().Err(err).Msg("读取客户端数据时出错")
	}

	log.Info().Str("client", conn.RemoteAddr().String()).Msg("客户端断开连接")
}

// smtpSession 表示一个SMTP会话
type smtpSession struct {
	conn net.Conn
	db   *db.DB
	cfg  *config.Config

	// 会话状态
	helo     string
	mailFrom string
	rcptTo   []string
	data     []string
	inData   bool
	quit     bool
}

// 创建新的SMTP会话
func newSession(conn net.Conn, database *db.DB, cfg *config.Config) *smtpSession {
	return &smtpSession{
		conn: conn,
		db:   database,
		cfg:  cfg,
	}
}

// 发送响应到客户端
func (s *smtpSession) send(message string) {
	fmt.Fprintf(s.conn, "%s\r\n", message)
}

// 处理SMTP命令
func (s *smtpSession) handleCommand(line string) error {
	log.Debug().Str("line", line).Msg("收到命令")

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

	// 提取邮件地址
	mailFrom := strings.ToLower(args[5:])
	mailFrom = strings.Trim(mailFrom, "<>")
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

	// 提取邮件地址
	rcptTo := strings.ToLower(args[3:])
	rcptTo = strings.Trim(rcptTo, "<>")
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
	s.send(statusDataReady)
	return nil
}

// 处理DATA内容
func (s *smtpSession) handleData(line string) error {
	// 数据结束标记
	if line == "." {
		s.inData = false

		// 处理邮件
		if err := s.processEmail(); err != nil {
			log.Error().Err(err).Msg("处理邮件时出错")
			s.send(fmt.Sprintf("554 Transaction failed: %s", err.Error()))
			return nil
		}

		s.send(statusOK)
		return nil
	}

	// 处理行首的点
	if strings.HasPrefix(line, ".") {
		line = line[1:]
	}

	s.data = append(s.data, line)
	return nil
}

// 处理RSET命令
func (s *smtpSession) handleRset() error {
	s.mailFrom = ""
	s.rcptTo = nil
	s.data = nil
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

	// 如果没有找到主题，使用默认主题
	if subject == "" {
		subject = "(无主题)"
	}

	// 将邮件添加到队列，保留完整的原始内容
	from := s.mailFrom
	if s.cfg.SMTPFrom != "" {
		from = s.cfg.SMTPFrom
	}

	// 保留原始邮件内容，包括所有邮件头和正文
	originalContent := strings.Join(s.data, "\r\n")

	_, err := s.db.QueueEmail(from, s.rcptTo, subject, originalContent)
	if err != nil {
		return fmt.Errorf("将邮件添加到队列时出错: %w", err)
	}

	// 重置会话状态
	s.mailFrom = ""
	s.rcptTo = nil
	s.data = nil

	return nil
}
