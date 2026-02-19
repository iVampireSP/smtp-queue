# SMTP Queue

SMTP Queue 是一个可横向扩展的 SMTP 中继队列服务，使用 Redis 存储邮件队列，支持多 SMTP 出站服务器和容灾切换。

## 特性

- 提供无需认证的 SMTP 服务器接口
- Redis 队列存储，支持多实例横向扩展
- 多 SMTP 出站服务器，加权随机 + 故障转移
- BRPOP 阻塞消费，天然防止重复发送
- 支持 TLS / SSL / 明文 连接
- 自动重试失败的邮件
- 邮件 TTL 自动过期清理
- 多平台 Docker 镜像（amd64 / arm64）

## 安装

### 从源码编译

```bash
git clone https://github.com/ivampiresp/smtp-queue.git
cd smtp-queue
go build -o smtp-queue .
```

### Docker

```bash
docker pull ghcr.io/ivampiresp/smtp-queue:latest

docker run -d \
  -p 2525:2525 \
  -v /path/to/config.yaml:/app/config.yaml \
  ghcr.io/ivampiresp/smtp-queue:latest
```

## 配置

复制示例配置并按需修改：

```bash
cp config.yaml.example config.yaml
```

配置文件路径通过以下优先级确定：
1. `-config` 命令行参数
2. `CONFIG_PATH` 环境变量
3. 默认 `config.yaml`

### 配置示例

```yaml
listen_addr: ":2525"

queue_interval: 30      # 秒
max_email_age: 72       # 小时
max_fail_count: 5
worker_concurrency: 5   # 每实例并发消费者数

redis:
  addrs:
    - "localhost:6379"
  # master_name: "mymaster"    # Sentinel 模式
  password: ""
  db: 0
  key_prefix: "smtp-queue:"

smtp_servers:
  - name: "primary"
    host: "smtp.example.com"
    port: 587
    username: "user@example.com"
    password: "your_password"
    from: "noreply@example.com"
    encryption: "tls"
    weight: 10
    enabled: true

  - name: "backup"
    host: "smtp-backup.example.com"
    port: 465
    username: "user@example.com"
    password: "your_password"
    from: "noreply@example.com"
    encryption: "ssl"
    weight: 1
    enabled: true
```

### 环境变量覆盖

敏感字段支持环境变量覆盖：

- `REDIS_PASSWORD` — 覆盖 `redis.password`
- `SMTP_{NAME}_PASSWORD` — 覆盖对应 name 的 SMTP 服务器密码（NAME 大写）
- `SMTP_{NAME}_USERNAME` — 覆盖对应 name 的 SMTP 服务器用户名（NAME 大写）

例如配置中 `name: "primary"` 的服务器，可通过 `SMTP_PRIMARY_PASSWORD` 环境变量覆盖密码。

## 使用

```bash
# 启动服务
./smtp-queue -config config.yaml

# 或使用环境变量指定配置
CONFIG_PATH=/etc/smtp-queue/config.yaml ./smtp-queue
```

### 横向扩展

多个实例连接同一个 Redis 即可实现横向扩展。BRPOP 保证同一封邮件只会被一个实例消费。

```bash
# 实例 1
./smtp-queue -config config.yaml

# 实例 2（可在不同机器）
./smtp-queue -config config.yaml
```

### 故障转移

配置多个 SMTP 服务器后，每封邮件发送时会按权重随机选择主服务器，若主服务器失败则依次尝试备选服务器。

## 测试

```bash
# 使用 telnet 测试
telnet localhost 2525

# SMTP 命令示例
HELO example.com
MAIL FROM:<sender@example.com>
RCPT TO:<recipient@example.com>
DATA
Subject: Test Email

This is a test email.
.
QUIT
```

## Docker Compose 示例

```yaml
services:
  redis:
    image: redis:7-alpine
    ports:
      - "6379:6379"

  smtp-queue:
    image: ghcr.io/ivampiresp/smtp-queue:latest
    ports:
      - "2525:2525"
    volumes:
      - ./config.yaml:/app/config.yaml
    depends_on:
      - redis
```

## 许可证

MIT
