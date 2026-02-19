package sender

import (
	"context"
	"fmt"
	"net/smtp"
	"strings"
	"time"

	"github.com/ivampiresp/smtp-queue/config"
	"github.com/ivampiresp/smtp-queue/pool"
	"github.com/ivampiresp/smtp-queue/queue"
)

// Send 通过连接池发送邮件
func Send(ctx context.Context, p *pool.Pool, email *queue.Email, server *config.SMTPServer) error {
	from := server.From
	if from == "" {
		return fmt.Errorf("SMTP 服务器 %s 未配置 from 地址", server.Name)
	}

	message := buildMessage(email, from)

	conn, err := p.Get(ctx, server)
	if err != nil {
		return err
	}

	if err := sendViaClient(conn.Client, from, email.To, []byte(message)); err != nil {
		p.Discard(conn)
		return err
	}

	// 发送成功后 RSET 重置连接状态，准备下次复用
	if err := conn.Client.Reset(); err != nil {
		p.Discard(conn)
		return nil // 邮件已发送成功，RSET 失败只是连接不可复用
	}

	p.Put(conn)
	return nil
}

// sanitizeHeaderValue 移除 header 值中的 CRLF 防止 header 注入
func sanitizeHeaderValue(v string) string {
	v = strings.ReplaceAll(v, "\r", "")
	v = strings.ReplaceAll(v, "\n", "")
	return v
}

// generateMessageID 生成唯一的 Message-ID
func generateMessageID(from string) string {
	domain := "localhost"
	if parts := strings.SplitN(from, "@", 2); len(parts) == 2 {
		domain = parts[1]
	}
	return fmt.Sprintf("<%d.%s@%s>", time.Now().UnixNano(), "smtp-queue", domain)
}

// buildMessage 构建邮件内容，处理 header 替换逻辑
func buildMessage(email *queue.Email, from string) string {
	lines := strings.Split(email.Body, "\r\n")
	if len(lines) <= 1 {
		lines = strings.Split(email.Body, "\n")
	}

	hasHeaders := false
	for _, line := range lines {
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "content-type:") ||
			strings.HasPrefix(lower, "mime-version:") {
			hasHeaders = true
			break
		}
	}

	if hasHeaders {
		return buildMessageWithExistingHeaders(lines, from, email.To)
	}
	return buildNewMessage(email, from)
}

// buildMessageWithExistingHeaders 替换或添加 From/To 头部
func buildMessageWithExistingHeaders(lines []string, from string, to []string) string {
	var newLines []string
	fromReplaced := false
	toReplaced := false
	hasMessageID := false

	for _, line := range lines {
		lower := strings.ToLower(line)

		if strings.HasPrefix(lower, "from:") {
			newLines = append(newLines, fmt.Sprintf("From: %s", sanitizeHeaderValue(from)))
			fromReplaced = true
			continue
		}

		if strings.HasPrefix(lower, "to:") {
			newLines = append(newLines, fmt.Sprintf("To: %s", sanitizeHeaderValue(buildAddressList(to))))
			toReplaced = true
			continue
		}

		if strings.HasPrefix(lower, "message-id:") {
			hasMessageID = true
		}

		newLines = append(newLines, line)
	}

	if !fromReplaced {
		newLines = insertBeforeEmptyLine(newLines, fmt.Sprintf("From: %s", sanitizeHeaderValue(from)))
	}

	if !toReplaced {
		newLines = insertBeforeEmptyLine(newLines, fmt.Sprintf("To: %s", sanitizeHeaderValue(buildAddressList(to))))
	}

	if !hasMessageID {
		newLines = insertBeforeEmptyLine(newLines, fmt.Sprintf("Message-ID: %s", generateMessageID(from)))
	}

	return strings.Join(newLines, "\r\n")
}

// buildNewMessage 构建包含完整头部的新邮件
func buildNewMessage(email *queue.Email, from string) string {
	headers := []struct{ Key, Value string }{
		{"From", sanitizeHeaderValue(from)},
		{"To", sanitizeHeaderValue(buildAddressList(email.To))},
		{"Subject", sanitizeHeaderValue(email.Subject)},
		{"Message-ID", generateMessageID(from)},
		{"Date", time.Now().Format(time.RFC1123Z)},
		{"MIME-Version", "1.0"},
		{"Content-Type", "text/plain; charset=\"utf-8\""},
		{"Content-Transfer-Encoding", "8bit"},
	}

	var msg string
	for _, h := range headers {
		msg += fmt.Sprintf("%s: %s\r\n", h.Key, h.Value)
	}
	msg += "\r\n" + email.Body
	return msg
}

// insertBeforeEmptyLine 在第一个空行前插入一行
func insertBeforeEmptyLine(lines []string, line string) []string {
	for i, l := range lines {
		if l == "" {
			result := make([]string, 0, len(lines)+1)
			result = append(result, lines[:i]...)
			result = append(result, line)
			result = append(result, lines[i:]...)
			return result
		}
	}
	return append([]string{line}, lines...)
}

// sendViaClient 通过已建立的 SMTP 客户端发送邮件
func sendViaClient(client *smtp.Client, from string, to []string, msg []byte) error {
	if err := client.Mail(from); err != nil {
		return fmt.Errorf("MAIL FROM: %w", err)
	}

	for _, addr := range to {
		if err := client.Rcpt(addr); err != nil {
			return fmt.Errorf("RCPT TO %s: %w", addr, err)
		}
	}

	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("DATA: %w", err)
	}

	if _, err = writer.Write(msg); err != nil {
		return fmt.Errorf("写入邮件数据: %w", err)
	}

	if err = writer.Close(); err != nil {
		return fmt.Errorf("提交邮件数据（邮件可能已发送）: %w", err)
	}

	return nil
}

// buildAddressList 构建地址列表字符串
func buildAddressList(addresses []string) string {
	return strings.Join(addresses, ", ")
}
