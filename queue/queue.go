package queue

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ivampiresp/smtp-queue/config"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
)

// Queue 基于 Redis 的邮件队列
type Queue struct {
	client  redis.UniversalClient
	prefix  string
	maxAge  time.Duration
	maxFail int
}

// New 创建一个新的 Redis 队列实例
func New(cfg *config.RedisConfig, maxAge time.Duration, maxFail int) (*Queue, error) {
	var client redis.UniversalClient

	if cfg.MasterName != "" {
		// Sentinel 模式
		client = redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName:    cfg.MasterName,
			SentinelAddrs: cfg.Addrs,
			Password:      cfg.Password,
			DB:            cfg.DB,
		})
	} else if len(cfg.Addrs) > 1 {
		// Cluster 模式
		if cfg.DB != 0 {
			log.Warn().Int("db", cfg.DB).Msg("Redis Cluster 不支持多数据库，忽略 db 配置")
		}
		client = redis.NewClusterClient(&redis.ClusterOptions{
			Addrs:    cfg.Addrs,
			Password: cfg.Password,
		})
	} else {
		// 单节点模式
		client = redis.NewClient(&redis.Options{
			Addr:     cfg.Addrs[0],
			Password: cfg.Password,
			DB:       cfg.DB,
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("连接 Redis 失败: %w", err)
	}

	// 确保 key prefix 包含 hash tag，兼容 Redis Cluster（所有 key 落在同一 slot）
	prefix := cfg.KeyPrefix
	if !strings.Contains(prefix, "{") {
		clean := strings.TrimSuffix(prefix, ":")
		prefix = "{" + clean + "}:"
	}

	return &Queue{
		client:  client,
		prefix:  prefix,
		maxAge:  maxAge,
		maxFail: maxFail,
	}, nil
}

// Close 关闭 Redis 连接
func (q *Queue) Close() error {
	return q.client.Close()
}

func (q *Queue) key(parts ...string) string {
	return q.prefix + strings.Join(parts, ":")
}

// Enqueue 将邮件入队（使用事务保证原子性）
func (q *Queue) Enqueue(ctx context.Context, from string, to []string, subject, body string) (string, error) {
	// 原子递增 ID
	id, err := q.client.Incr(ctx, q.key("id_counter")).Result()
	if err != nil {
		return "", fmt.Errorf("生成邮件 ID: %w", err)
	}

	emailID := strconv.FormatInt(id, 10)
	emailKey := q.key("email", emailID)

	pipe := q.client.TxPipeline()

	pipe.HSet(ctx, emailKey, map[string]interface{}{
		"id":         emailID,
		"from":       from,
		"to":         strings.Join(to, ";"),
		"subject":    subject,
		"body":       body,
		"created_at": time.Now().Unix(),
		"fail_count": 0,
		"last_error": "",
	})

	pipe.Expire(ctx, emailKey, q.maxAge)
	pipe.LPush(ctx, q.key("queue", "pending"), emailID)

	if _, err := pipe.Exec(ctx); err != nil {
		return "", fmt.Errorf("入队邮件: %w", err)
	}

	return emailID, nil
}

// Dequeue 从队列中取出一封邮件
// 使用 BLMOVE 原子地将 ID 从 pending 移到 processing，防止崩溃丢失
func (q *Queue) Dequeue(ctx context.Context) (*Email, error) {
	emailID, err := q.client.BLMove(ctx,
		q.key("queue", "pending"),
		q.key("queue", "processing"),
		"RIGHT", "LEFT",
		5*time.Second,
	).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, nil // 超时，无邮件
		}
		return nil, fmt.Errorf("BLMOVE: %w", err)
	}

	email, err := q.getEmail(ctx, emailID)
	if err != nil {
		return nil, err
	}

	// hash 已过期，从 processing 中清除这个孤儿 ID
	if email == nil {
		q.client.LRem(ctx, q.key("queue", "processing"), 1, emailID)
		log.Debug().Str("id", emailID).Msg("邮件 hash 已过期，跳过")
		return nil, nil
	}

	return email, nil
}

// getEmail 从 Redis hash 获取邮件数据
func (q *Queue) getEmail(ctx context.Context, id string) (*Email, error) {
	emailKey := q.key("email", id)

	data, err := q.client.HGetAll(ctx, emailKey).Result()
	if err != nil {
		return nil, fmt.Errorf("获取邮件 %s: %w", id, err)
	}

	// hash 已过期或不存在
	if len(data) == 0 {
		return nil, nil
	}

	createdUnix, _ := strconv.ParseInt(data["created_at"], 10, 64)
	failCount, _ := strconv.Atoi(data["fail_count"])

	var to []string
	if data["to"] != "" {
		to = strings.Split(data["to"], ";")
	}

	return &Email{
		ID:        data["id"],
		From:      data["from"],
		To:        to,
		Subject:   data["subject"],
		Body:      data["body"],
		CreatedAt: time.Unix(createdUnix, 0),
		FailCount: failCount,
		LastError: data["last_error"],
	}, nil
}

// Delete 删除邮件（发送成功后调用），同时从 processing 队列移除
func (q *Queue) Delete(ctx context.Context, id string) error {
	pipe := q.client.TxPipeline()
	pipe.Del(ctx, q.key("email", id))
	pipe.LRem(ctx, q.key("queue", "processing"), 1, id)
	_, err := pipe.Exec(ctx)
	return err
}

// markFailedScript 原子性地标记失败：递增计数 + 更新错误 + 重新入队或删除
var markFailedScript = redis.NewScript(`
local emailKey = KEYS[1]
local pendingKey = KEYS[2]
local processingKey = KEYS[3]
local errMsg = ARGV[1]
local maxFail = tonumber(ARGV[2])
local emailID = ARGV[3]
local ttlSeconds = tonumber(ARGV[4])

local newCount = redis.call('HINCRBY', emailKey, 'fail_count', 1)
redis.call('HSET', emailKey, 'last_error', errMsg)
redis.call('LREM', processingKey, 1, emailID)

if newCount < maxFail then
    redis.call('EXPIRE', emailKey, ttlSeconds)
    redis.call('RPUSH', pendingKey, emailID)
    return newCount
else
    redis.call('DEL', emailKey)
    return -1
end
`)

// MarkFailed 标记邮件发送失败（原子操作）
// 失败次数未达上限时刷新 TTL 并重新入队（RPush 到队尾实现自然退避）
// 超过上限则删除邮件
func (q *Queue) MarkFailed(ctx context.Context, id string, errMsg string) error {
	result, err := markFailedScript.Run(ctx, q.client,
		[]string{
			q.key("email", id),
			q.key("queue", "pending"),
			q.key("queue", "processing"),
		},
		errMsg,
		q.maxFail,
		id,
		int(q.maxAge.Seconds()),
	).Int()

	if err != nil {
		return fmt.Errorf("标记失败: %w", err)
	}

	if result == -1 {
		log.Warn().Str("id", id).Msg("邮件超过最大失败次数，已删除")
	}

	return nil
}

// ReclaimStale 回收 processing 队列中卡住的邮件
// 建议由定时任务调用，将超时的邮件重新放回 pending 队列
func (q *Queue) ReclaimStale(ctx context.Context, staleTimeout time.Duration) (int, error) {
	ids, err := q.client.LRange(ctx, q.key("queue", "processing"), 0, -1).Result()
	if err != nil {
		return 0, err
	}

	reclaimed := 0
	for _, id := range ids {
		emailKey := q.key("email", id)
		exists, err := q.client.Exists(ctx, emailKey).Result()
		if err != nil {
			continue
		}

		if exists == 0 {
			// hash 已过期，清理 processing 中的孤儿 ID
			q.client.LRem(ctx, q.key("queue", "processing"), 1, id)
			reclaimed++
			continue
		}

		// 检查邮件创建时间，如果在 processing 中停留过久则回收
		createdStr, err := q.client.HGet(ctx, emailKey, "created_at").Result()
		if err != nil {
			continue
		}
		createdUnix, _ := strconv.ParseInt(createdStr, 10, 64)
		created := time.Unix(createdUnix, 0)

		// 如果邮件本身已过期，清理即可
		if time.Since(created) > q.maxAge {
			pipe := q.client.TxPipeline()
			pipe.LRem(ctx, q.key("queue", "processing"), 1, id)
			pipe.Del(ctx, emailKey)
			pipe.Exec(ctx)
			reclaimed++
		}
	}

	return reclaimed, nil
}
