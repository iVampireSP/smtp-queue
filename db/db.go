package db

import (
	"database/sql"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Email 代表队列中的一封电子邮件
type Email struct {
	ID        int64
	From      string
	To        []string
	Subject   string
	Body      string
	Created   time.Time
	Sent      bool
	SentAt    *time.Time
	FailCount int
	LastError string
}

// DB 是数据库操作的包装器
type DB struct {
	db *sql.DB
}

// Init 初始化数据库连接并确保表已创建
func Init(dbPath string) (*DB, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}

	// 创建邮件队列表
	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS emails (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		from_address TEXT NOT NULL,
		to_addresses TEXT NOT NULL,
		subject TEXT NOT NULL,
		body TEXT NOT NULL,
		created_at TIMESTAMP NOT NULL,
		sent BOOLEAN NOT NULL DEFAULT 0,
		sent_at TIMESTAMP,
		fail_count INTEGER NOT NULL DEFAULT 0,
		last_error TEXT
	)`)
	if err != nil {
		return nil, err
	}

	return &DB{db: db}, nil
}

// Close 关闭数据库连接
func (d *DB) Close() error {
	return d.db.Close()
}

// QueueEmail 将邮件添加到队列中
func (d *DB) QueueEmail(from string, to []string, subject, body string) (int64, error) {
	// 将收件人列表序列化为字符串
	toStr := ""
	for i, addr := range to {
		if i > 0 {
			toStr += ";"
		}
		toStr += addr
	}

	result, err := d.db.Exec(
		"INSERT INTO emails (from_address, to_addresses, subject, body, created_at) VALUES (?, ?, ?, ?, ?)",
		from, toStr, subject, body, time.Now(),
	)
	if err != nil {
		return 0, err
	}

	return result.LastInsertId()
}

// GetPendingEmails 获取等待发送的邮件
func (d *DB) GetPendingEmails(limit int) ([]*Email, error) {
	rows, err := d.db.Query(`
		SELECT id, from_address, to_addresses, subject, body, created_at, fail_count, last_error
		FROM emails
		WHERE sent = 0
		ORDER BY created_at ASC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var emails []*Email
	for rows.Next() {
		var (
			id        int64
			from      string
			toStr     string
			subject   string
			body      string
			createdAt time.Time
			failCount int
			lastError sql.NullString
		)

		if err := rows.Scan(&id, &from, &toStr, &subject, &body, &createdAt, &failCount, &lastError); err != nil {
			return nil, err
		}

		// 解析收件人列表
		to := splitAddresses(toStr)

		lastErrorStr := ""
		if lastError.Valid {
			lastErrorStr = lastError.String
		}

		emails = append(emails, &Email{
			ID:        id,
			From:      from,
			To:        to,
			Subject:   subject,
			Body:      body,
			Created:   createdAt,
			Sent:      false,
			FailCount: failCount,
			LastError: lastErrorStr,
		})
	}

	return emails, rows.Err()
}

// MarkEmailSent 将邮件标记为已发送
func (d *DB) MarkEmailSent(id int64) error {
	_, err := d.db.Exec(
		"UPDATE emails SET sent = 1, sent_at = ? WHERE id = ?",
		time.Now(), id,
	)
	return err
}

// DeleteEmail 从数据库中删除邮件
func (d *DB) DeleteEmail(id int64) error {
	_, err := d.db.Exec("DELETE FROM emails WHERE id = ?", id)
	return err
}

// MarkEmailFailed 标记邮件发送失败并增加失败计数
func (d *DB) MarkEmailFailed(id int64, errorMsg string) error {
	_, err := d.db.Exec(
		"UPDATE emails SET fail_count = fail_count + 1, last_error = ? WHERE id = ?",
		errorMsg, id,
	)
	return err
}

// CleanupOldEmails 清理过老的邮件
func (d *DB) CleanupOldEmails(maxAge time.Duration, maxFailCount int) (int64, error) {
	// 删除超过最大失败次数的邮件
	failResult, err := d.db.Exec(
		"DELETE FROM emails WHERE fail_count >= ?",
		maxFailCount,
	)
	if err != nil {
		return 0, err
	}

	// 删除过老的邮件
	oldTime := time.Now().Add(-maxAge)
	ageResult, err := d.db.Exec(
		"DELETE FROM emails WHERE created_at < ?",
		oldTime,
	)
	if err != nil {
		return 0, err
	}

	// 计算总共删除的邮件数
	failCount, _ := failResult.RowsAffected()
	ageCount, _ := ageResult.RowsAffected()

	return failCount + ageCount, nil
}

// 辅助函数：拆分地址字符串
func splitAddresses(addresses string) []string {
	if addresses == "" {
		return []string{}
	}

	var result []string
	for _, addr := range splitString(addresses, ";") {
		if addr != "" {
			result = append(result, addr)
		}
	}
	return result
}

// 辅助函数：按分隔符拆分字符串
func splitString(s, sep string) []string {
	if s == "" {
		return []string{}
	}

	var result []string
	start := 0
	for i := 0; i < len(s); i++ {
		if i+len(sep) <= len(s) && s[i:i+len(sep)] == sep {
			result = append(result, s[start:i])
			start = i + len(sep)
			i += len(sep) - 1
		}
	}
	if start < len(s) {
		result = append(result, s[start:])
	}
	return result
}
