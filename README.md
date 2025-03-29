# SMTP Queue

SMTP Queue 是一个简单的SMTP服务器，它接收邮件并将其添加到队列中，然后通过配置的真实SMTP服务器发送。

## 特性

- 提供无需认证的SMTP服务器接口
- 将接收到的邮件保存到SQLite数据库
- 定时发送队列中的邮件
- 支持TLS连接
- 自动重试失败的邮件
- 发送成功后自动删除邮件
- 定期清理过期或多次失败的邮件

## 安装

```bash
# 克隆项目
git clone https://github.com/ivampiresp/smtp-queue.git
cd smtp-queue

# 安装依赖
go mod download

# 编译项目
go build -o smtp-queue
```

## 配置

项目使用.env文件进行配置，您可以复制 `.env.example` 文件并根据需要修改：

```bash
cp .env.example .env
```

配置选项：

- `LISTEN_ADDR`: SMTP服务器监听地址，例如:1025
- `DB_PATH`: SQLite数据库文件路径
- `QUEUE_INTERVAL`: 队列处理间隔（秒）
- `MAX_EMAIL_AGE`: 邮件最大保留时间（小时）
- `MAX_FAIL_COUNT`: 邮件最大失败尝试次数
- `SMTP_HOST`: 真实SMTP服务器主机
- `SMTP_PORT`: 真实SMTP服务器端口
- `SMTP_USERNAME`: SMTP用户名
- `SMTP_PASSWORD`: SMTP密码
- `SMTP_FROM`: 发件人地址（覆盖客户端提供的地址）
- `SMTP_ENCRYPTION`: SMTP加密方式，支持：none(无加密)、ssl、tls

## 使用

```bash
# 启动服务器
./smtp-queue
```

客户端可以连接到配置的监听地址，无需认证即可发送邮件。服务器会将邮件存入队列，然后使用配置的SMTP服务器发送。成功发送后，邮件会自动从队列中删除。

## 数据库管理

系统会自动管理队列：

- 成功发送的邮件会立即从数据库中删除
- 失败次数超过`MAX_FAIL_COUNT`的邮件会被自动清理
- 创建时间超过`MAX_EMAIL_AGE`小时的邮件会被自动清理
- 清理任务每12小时自动执行一次

## 测试

可以使用以下命令测试服务器：

```bash
# 使用telnet测试
telnet localhost 1025

# SMTP命令示例
HELO example.com
MAIL FROM:<sender@example.com>
RCPT TO:<recipient@example.com>
DATA
Subject: Test Email

This is a test email.
.
QUIT
```

## 许可证

MIT 