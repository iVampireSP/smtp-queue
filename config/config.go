package config

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 包含应用程序的配置
type Config struct {
	ListenAddr        string       `yaml:"listen_addr"`
	Banner            string       `yaml:"banner"`
	QueueIntervalSec  int          `yaml:"queue_interval"`
	MaxEmailAgeHours  int          `yaml:"max_email_age"`
	MaxFailCount      int          `yaml:"max_fail_count"`
	WorkerConcurrency int          `yaml:"worker_concurrency"`
	Redis             RedisConfig  `yaml:"redis"`
	Pool              PoolConfig   `yaml:"pool"`
	SMTPServers       []SMTPServer `yaml:"smtp_servers"`

	// 派生字段
	QueueInterval time.Duration `yaml:"-"`
	MaxEmailAge   time.Duration `yaml:"-"`
}

// RedisConfig Redis 连接配置
type RedisConfig struct {
	Addrs      []string `yaml:"addrs"`
	MasterName string   `yaml:"master_name"`
	Password   string   `yaml:"password"`
	DB         int      `yaml:"db"`
	KeyPrefix  string   `yaml:"key_prefix"`
}

// SMTPServer 出站 SMTP 服务器配置
type SMTPServer struct {
	Name       string `yaml:"name"`
	Host       string `yaml:"host"`
	Port       int    `yaml:"port"`
	Username   string `yaml:"username"`
	Password   string `yaml:"password"`
	From       string `yaml:"from"`
	Encryption string `yaml:"encryption"` // none, ssl, tls
	Weight     int    `yaml:"weight"`
	Enabled    bool   `yaml:"enabled"`
}

// PoolConfig SMTP 连接池配置
type PoolConfig struct {
	MaxIdlePerServer   int `yaml:"max_idle_per_server"`
	MaxActivePerServer int `yaml:"max_active_per_server"`
	MaxIdleTimeSec     int `yaml:"max_idle_time"`
	MaxLifetimeSec     int `yaml:"max_lifetime"`
	MaxMessagesPerConn int `yaml:"max_messages_per_conn"`
	DialTimeoutSec     int `yaml:"dial_timeout"`
}

// Load 从 YAML 文件加载配置
func Load() (*Config, error) {
	configPath := getConfigPath()

	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件 %s: %w", configPath, err)
	}

	cfg := &Config{
		// 默认值
		ListenAddr:        ":2525",
		Banner:            "SMTP Queue Server Ready",
		QueueIntervalSec:  30,
		MaxEmailAgeHours:  72,
		MaxFailCount:      5,
		WorkerConcurrency: 5,
		Redis: RedisConfig{
			Addrs:     []string{"localhost:6379"},
			DB:        0,
			KeyPrefix: "smtp-queue:",
		},
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件: %w", err)
	}

	// 环境变量覆盖敏感字段
	applyEnvOverrides(cfg)

	// 规范化加密方式
	for i := range cfg.SMTPServers {
		switch strings.ToLower(cfg.SMTPServers[i].Encryption) {
		case "ssl":
			cfg.SMTPServers[i].Encryption = "ssl"
		case "tls":
			cfg.SMTPServers[i].Encryption = "tls"
		default:
			cfg.SMTPServers[i].Encryption = "none"
		}
	}

	// 计算派生字段
	cfg.QueueInterval = time.Duration(cfg.QueueIntervalSec) * time.Second
	cfg.MaxEmailAge = time.Duration(cfg.MaxEmailAgeHours) * time.Hour

	// 基本验证
	if len(cfg.SMTPServers) == 0 {
		return nil, fmt.Errorf("至少需要配置一个 SMTP 服务器")
	}
	if len(cfg.Redis.Addrs) == 0 {
		return nil, fmt.Errorf("至少需要配置一个 Redis 地址")
	}

	return cfg, nil
}

// getConfigPath 获取配置文件路径：-config flag > CONFIG_PATH 环境变量 > 默认 config.yaml
func getConfigPath() string {
	var configPath string
	flag.StringVar(&configPath, "config", "", "配置文件路径")
	flag.Parse()

	if configPath != "" {
		return configPath
	}

	if envPath := os.Getenv("CONFIG_PATH"); envPath != "" {
		return envPath
	}

	return "config.yaml"
}

// applyEnvOverrides 使用环境变量覆盖敏感字段
func applyEnvOverrides(cfg *Config) {
	// REDIS_PASSWORD 覆盖 redis.password
	if v := os.Getenv("REDIS_PASSWORD"); v != "" {
		cfg.Redis.Password = v
	}

	// SMTP_{NAME}_PASSWORD / SMTP_{NAME}_USERNAME 覆盖对应服务器的密码/用户名
	for i := range cfg.SMTPServers {
		name := strings.ToUpper(cfg.SMTPServers[i].Name)
		if name == "" {
			continue
		}

		if v := os.Getenv("SMTP_" + name + "_PASSWORD"); v != "" {
			cfg.SMTPServers[i].Password = v
		}
		if v := os.Getenv("SMTP_" + name + "_USERNAME"); v != "" {
			cfg.SMTPServers[i].Username = v
		}
	}
}
