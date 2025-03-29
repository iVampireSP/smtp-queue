package config

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Config 包含应用程序的配置
type Config struct {
	// SMTP服务器监听地址
	ListenAddr string

	// 数据库文件路径
	DBPath string

	// 队列处理间隔
	QueueInterval time.Duration

	// 邮件清理配置
	MaxEmailAge  time.Duration
	MaxFailCount int

	// SMTP服务器配置
	SMTPHost       string
	SMTPPort       int
	SMTPUsername   string
	SMTPPassword   string
	SMTPFrom       string
	SMTPEncryption string // 加密方式: none, ssl, tls
}

// Load 从.env文件加载配置
func Load() (*Config, error) {
	if err := godotenv.Load(); err != nil {
		return nil, err
	}

	queueInterval, err := strconv.Atoi(getEnv("QUEUE_INTERVAL", "30"))
	if err != nil {
		queueInterval = 30
	}

	maxEmailAge, err := strconv.Atoi(getEnv("MAX_EMAIL_AGE", "72"))
	if err != nil {
		maxEmailAge = 72
	}

	maxFailCount, err := strconv.Atoi(getEnv("MAX_FAIL_COUNT", "5"))
	if err != nil {
		maxFailCount = 5
	}

	smtpPort, err := strconv.Atoi(getEnv("SMTP_PORT", "587"))
	if err != nil {
		smtpPort = 587
	}

	// 获取加密方式
	smtpEncryption := getEnv("SMTP_ENCRYPTION", "tls")
	// 规范化加密方式
	switch strings.ToLower(smtpEncryption) {
	case "ssl":
		smtpEncryption = "ssl"
	case "tls":
		smtpEncryption = "tls"
	default:
		smtpEncryption = "none"
	}

	return &Config{
		ListenAddr:     getEnv("LISTEN_ADDR", ":1025"),
		DBPath:         getEnv("DB_PATH", "./smtp_queue.db"),
		QueueInterval:  time.Duration(queueInterval) * time.Second,
		MaxEmailAge:    time.Duration(maxEmailAge) * time.Hour,
		MaxFailCount:   maxFailCount,
		SMTPHost:       getEnv("SMTP_HOST", ""),
		SMTPPort:       smtpPort,
		SMTPUsername:   getEnv("SMTP_USERNAME", ""),
		SMTPPassword:   getEnv("SMTP_PASSWORD", ""),
		SMTPFrom:       getEnv("SMTP_FROM", ""),
		SMTPEncryption: smtpEncryption,
	}, nil
}

// getEnv 获取环境变量，如果不存在则返回默认值
func getEnv(key, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	return value
}
