package worker

import (
	"context"
	"sync"

	"github.com/ivampiresp/smtp-queue/config"
	"github.com/ivampiresp/smtp-queue/pool"
	"github.com/ivampiresp/smtp-queue/queue"
	"github.com/ivampiresp/smtp-queue/selector"
	"github.com/ivampiresp/smtp-queue/sender"
	"github.com/rs/zerolog/log"
)

// Worker 负责从 Redis 队列消费邮件并发送
type Worker struct {
	queue  *queue.Queue
	config *config.Config
	pool   *pool.Pool
}

// New 创建一个新的 Worker 实例
func New(q *queue.Queue, cfg *config.Config, p *pool.Pool) *Worker {
	return &Worker{
		queue:  q,
		config: cfg,
		pool:   p,
	}
}

// Start 启动 N 个消费者 goroutine
func (w *Worker) Start(ctx context.Context) {
	concurrency := w.config.WorkerConcurrency
	if concurrency <= 0 {
		concurrency = 1
	}

	log.Info().Int("concurrency", concurrency).Msg("邮件队列工作者已启动")

	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			w.consumeLoop(ctx, id)
		}(i)
	}

	wg.Wait()
	log.Info().Msg("邮件队列工作者已停止")
}

// consumeLoop 单个消费者的循环
func (w *Worker) consumeLoop(ctx context.Context, workerID int) {
	logger := log.With().Int("worker_id", workerID).Logger()
	logger.Info().Msg("消费者已启动")

	for {
		select {
		case <-ctx.Done():
			logger.Info().Msg("消费者正在停止")
			return
		default:
		}

		email, err := w.queue.Dequeue(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logger.Error().Err(err).Msg("从队列取出邮件失败")
			continue
		}

		if email == nil {
			continue
		}

		logger.Info().
			Str("id", email.ID).
			Str("from", email.From).
			Strs("to", email.To).
			Str("subject", email.Subject).
			Msg("正在发送邮件")

		servers := selector.SelectWithFailover(w.config.SMTPServers)
		if len(servers) == 0 {
			logger.Error().Str("id", email.ID).Msg("没有可用的 SMTP 服务器")
			if err := w.queue.MarkFailed(ctx, email.ID, "没有可用的 SMTP 服务器"); err != nil {
				logger.Error().Err(err).Str("id", email.ID).Msg("标记失败时出错")
			}
			continue
		}

		// 依次尝试发送（通过连接池）
		sent := false
		for _, srv := range servers {
			if err := sender.Send(ctx, w.pool, email, &srv); err != nil {
				logger.Warn().
					Err(err).
					Str("id", email.ID).
					Str("server", srv.Name).
					Msg("通过此服务器发送失败，尝试下一个")
				continue
			}
			sent = true
			logger.Info().
				Str("id", email.ID).
				Str("server", srv.Name).
				Msg("邮件发送成功")
			break
		}

		if sent {
			if err := w.queue.Delete(ctx, email.ID); err != nil {
				logger.Error().Err(err).Str("id", email.ID).Msg("删除已发送邮件时出错")
			}
		} else {
			logger.Error().Str("id", email.ID).Msg("所有 SMTP 服务器均发送失败")
			if err := w.queue.MarkFailed(ctx, email.ID, "所有 SMTP 服务器均发送失败"); err != nil {
				logger.Error().Err(err).Str("id", email.ID).Msg("标记失败时出错")
			}
		}
	}
}
