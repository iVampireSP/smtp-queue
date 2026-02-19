package queue

import "time"

// Email 代表队列中的一封电子邮件
type Email struct {
	ID        string
	From      string
	To        []string
	Subject   string
	Body      string
	CreatedAt time.Time
	FailCount int
	LastError string
}
