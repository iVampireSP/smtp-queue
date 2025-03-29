package worker

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"

	"github.com/ivampiresp/smtp-queue/config"
	"github.com/ivampiresp/smtp-queue/db"
	"github.com/rs/zerolog/log"
)

// Worker 负责处理队列中的邮件并发送它们
type Worker struct {
	db     *db.DB
	config *config.Config
}

// New 创建一个新的Worker实例
func New(database *db.DB, cfg *config.Config) *Worker {
	return &Worker{
		db:     database,
		config: cfg,
	}
}

// Start 开始处理邮件队列
func (w *Worker) Start(ctx context.Context) {
	log.Info().Dur("interval", w.config.QueueInterval).Msg("邮件队列工作者已启动")

	ticker := time.NewTicker(w.config.QueueInterval)
	defer ticker.Stop()

	// 创建清理任务定时器（每12小时执行一次）
	cleanupTicker := time.NewTicker(12 * time.Hour)
	defer cleanupTicker.Stop()

	// 立即处理一次队列
	w.processQueue()

	// 立即执行一次清理
	w.cleanupOldEmails()

	for {
		select {
		case <-ticker.C:
			w.processQueue()
		case <-cleanupTicker.C:
			w.cleanupOldEmails()
		case <-ctx.Done():
			log.Info().Msg("邮件队列工作者正在停止")
			return
		}
	}
}

// 清理过老或失败次数过多的邮件
func (w *Worker) cleanupOldEmails() {
	log.Debug().Msg("清理过老的邮件")

	count, err := w.db.CleanupOldEmails(w.config.MaxEmailAge, w.config.MaxFailCount)
	if err != nil {
		log.Error().Err(err).Msg("清理邮件时出错")
		return
	}

	if count > 0 {
		log.Info().Int64("count", count).Msg("已清理过期或失败的邮件")
	}
}

// 处理队列中的邮件
func (w *Worker) processQueue() {
	log.Debug().Msg("处理邮件队列")

	// 每次最多处理 10 封邮件
	emails, err := w.db.GetPendingEmails(10)
	if err != nil {
		log.Error().Err(err).Msg("获取待处理邮件时出错")
		return
	}

	if len(emails) == 0 {
		log.Debug().Msg("队列中没有待处理邮件")
		return
	}

	log.Info().Int("count", len(emails)).Msg("发现待处理的邮件")

	for _, email := range emails {
		log.Info().
			Int64("id", email.ID).
			Str("from", email.From).
			Strs("to", email.To).
			Str("subject", email.Subject).
			Msg("正在发送邮件")

		if err := w.sendEmail(email); err != nil {
			log.Error().Err(err).Int64("id", email.ID).Msg("发送邮件失败")

			// 更新失败计数
			if err := w.db.MarkEmailFailed(email.ID, err.Error()); err != nil {
				log.Error().Err(err).Int64("id", email.ID).Msg("更新邮件失败状态时出错")
			}

			// 如果失败次数太多，可以考虑放弃此邮件
			if email.FailCount >= 5 {
				log.Warn().Int64("id", email.ID).Msg("邮件失败次数过多，删除邮件")
				if err := w.db.DeleteEmail(email.ID); err != nil {
					log.Error().Err(err).Int64("id", email.ID).Msg("删除失败的邮件时出错")
				}
			}

			continue
		}

		// 删除已发送的邮件
		if err := w.db.DeleteEmail(email.ID); err != nil {
			log.Error().Err(err).Int64("id", email.ID).Msg("删除已发送邮件时出错")
			continue
		}

		log.Info().Int64("id", email.ID).Msg("邮件发送成功并已从队列中删除")
	}
}

// 发送单封邮件
func (w *Worker) sendEmail(email *db.Email) error {
	// 检查SMTP配置
	if w.config.SMTPHost == "" {
		return fmt.Errorf("未配置SMTP服务器")
	}

	// 准备SMTP服务器地址和认证信息
	smtpAddr := fmt.Sprintf("%s:%d", w.config.SMTPHost, w.config.SMTPPort)
	auth := smtp.PlainAuth("", w.config.SMTPUsername, w.config.SMTPPassword, w.config.SMTPHost)

	// 始终使用配置的SMTP_FROM作为发件人，忽略客户端提供的发件人
	from := w.config.SMTPFrom
	if from == "" {
		return fmt.Errorf("未配置SMTP_FROM，无法发送邮件")
	}

	// 检查邮件内容是否已包含邮件头
	hasHeaders := false
	lines := strings.Split(email.Body, "\r\n")
	if len(lines) == 0 {
		lines = strings.Split(email.Body, "\n") // 兼容以\n分隔的情况
	}

	// 检查是否存在常见的邮件头
	for _, line := range lines {
		if strings.HasPrefix(strings.ToLower(line), "content-type:") ||
			strings.HasPrefix(strings.ToLower(line), "mime-version:") {
			hasHeaders = true
			break
		}
	}

	// 准备邮件内容
	var message string
	if hasHeaders {
		// 邮件内容已经包含头部，我们需要替换或添加From头部
		var newLines []string
		fromReplaced := false
		toReplaced := false

		// 替换或添加必要的头部
		for _, line := range lines {
			lowerLine := strings.ToLower(line)

			// 替换From头
			if strings.HasPrefix(lowerLine, "from:") {
				newLines = append(newLines, fmt.Sprintf("From: %s", from))
				fromReplaced = true
				continue
			}

			// 替换To头
			if strings.HasPrefix(lowerLine, "to:") {
				newLines = append(newLines, fmt.Sprintf("To: %s", buildAddressList(email.To)))
				toReplaced = true
				continue
			}

			// 保留其他头部和内容
			newLines = append(newLines, line)
		}

		// 如果没有替换From头，添加一个
		if !fromReplaced {
			// 找到第一个空行前插入From头
			inserted := false
			newLinesWithFrom := []string{}

			for _, line := range newLines {
				if line == "" && !inserted {
					newLinesWithFrom = append(newLinesWithFrom, fmt.Sprintf("From: %s", from))
					inserted = true
				}
				newLinesWithFrom = append(newLinesWithFrom, line)
			}

			// 如果没有空行，在开头添加From头
			if !inserted {
				newLinesWithFrom = append([]string{fmt.Sprintf("From: %s", from)}, newLines...)
			}

			newLines = newLinesWithFrom
		}

		// 如果没有替换To头，添加一个
		if !toReplaced {
			// 找到第一个空行前插入To头
			inserted := false
			newLinesWith := []string{}

			for _, line := range newLines {
				if line == "" && !inserted {
					newLinesWith = append(newLinesWith, fmt.Sprintf("To: %s", buildAddressList(email.To)))
					inserted = true
				}
				newLinesWith = append(newLinesWith, line)
			}

			// 如果没有空行，在开头添加To头
			if !inserted {
				newLinesWith = append([]string{fmt.Sprintf("To: %s", buildAddressList(email.To))}, newLines...)
			}

			newLines = newLinesWith
		}

		message = strings.Join(newLines, "\r\n")
	} else {
		// 构建完整的邮件，包括头部
		header := make(map[string]string)
		header["From"] = from
		header["To"] = buildAddressList(email.To)
		header["Subject"] = email.Subject
		header["MIME-Version"] = "1.0"
		header["Content-Type"] = "text/plain; charset=\"utf-8\""
		header["Content-Transfer-Encoding"] = "8bit"
		header["Date"] = time.Now().Format(time.RFC1123Z)

		message = ""
		for k, v := range header {
			message += fmt.Sprintf("%s: %s\r\n", k, v)
		}
		message += "\r\n" + email.Body
	}

	// 根据加密方式发送邮件
	switch w.config.SMTPEncryption {
	case "tls":
		return w.sendMailWithTLS(smtpAddr, auth, from, email.To, []byte(message))
	case "ssl":
		return w.sendMailWithSSL(smtpAddr, auth, from, email.To, []byte(message))
	default:
		// 无加密
		return smtp.SendMail(smtpAddr, auth, from, email.To, []byte(message))
	}
}

// 使用TLS发送邮件（先连接后加密）
func (w *Worker) sendMailWithTLS(addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
	// 解析服务器地址
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}

	// 先连接到服务器
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()

	// 创建SMTP客户端
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer client.Close()

	// 开始TLS加密
	tlsConfig := &tls.Config{
		ServerName: host,
	}
	if err = client.StartTLS(tlsConfig); err != nil {
		return err
	}

	// 认证
	if auth != nil {
		if err = client.Auth(auth); err != nil {
			return err
		}
	}

	// 设置发件人
	if err = client.Mail(from); err != nil {
		return err
	}

	// 设置收件人
	for _, addr := range to {
		if err = client.Rcpt(addr); err != nil {
			return err
		}
	}

	// 发送邮件主体
	writer, err := client.Data()
	if err != nil {
		return err
	}

	_, err = writer.Write(msg)
	if err != nil {
		return err
	}

	err = writer.Close()
	if err != nil {
		return err
	}

	return client.Quit()
}

// 使用SSL发送邮件（直接使用TLS连接）
func (w *Worker) sendMailWithSSL(addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
	// 解析服务器地址
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}

	// TLS配置
	tlsConfig := &tls.Config{
		ServerName: host,
	}

	// 直接使用TLS连接
	conn, err := tls.Dial("tcp", addr, tlsConfig)
	if err != nil {
		return err
	}
	defer conn.Close()

	// 创建SMTP客户端
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer client.Close()

	// 认证
	if auth != nil {
		if err = client.Auth(auth); err != nil {
			return err
		}
	}

	// 设置发件人
	if err = client.Mail(from); err != nil {
		return err
	}

	// 设置收件人
	for _, addr := range to {
		if err = client.Rcpt(addr); err != nil {
			return err
		}
	}

	// 发送邮件主体
	writer, err := client.Data()
	if err != nil {
		return err
	}

	_, err = writer.Write(msg)
	if err != nil {
		return err
	}

	err = writer.Close()
	if err != nil {
		return err
	}

	return client.Quit()
}

// 构建地址列表
func buildAddressList(addresses []string) string {
	if len(addresses) == 0 {
		return ""
	}

	result := addresses[0]
	for i := 1; i < len(addresses); i++ {
		result += ", " + addresses[i]
	}
	return result
}
