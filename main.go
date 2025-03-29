package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ivampiresp/smtp-queue/config"
	"github.com/ivampiresp/smtp-queue/db"
	"github.com/ivampiresp/smtp-queue/server"
	"github.com/ivampiresp/smtp-queue/worker"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func main() {
	// 初始化日志
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})

	// 加载配置
	cfg, err := config.Load()
	if err != nil {
		log.Fatal().Err(err).Msg("无法加载配置")
	}

	// 初始化数据库
	database, err := db.Init(cfg.DBPath)
	if err != nil {
		log.Fatal().Err(err).Msg("无法初始化数据库")
	}
	defer database.Close()

	// 创建上下文，用于优雅关闭
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 启动工作者（负责发送队列中的邮件）
	w := worker.New(database, cfg)
	go w.Start(ctx)

	// 启动SMTP服务器
	s := server.New(database, cfg)
	go func() {
		if err := s.Start(); err != nil {
			log.Error().Err(err).Msg("SMTP服务器错误")
			cancel()
		}
	}()

	log.Info().
		Str("listen_addr", cfg.ListenAddr).
		Msg("SMTP队列服务器已启动")

	// 等待中断信号以优雅地关闭服务器
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Info().Msg("正在关闭服务...")
	cancel()

	// 关闭SMTP服务器
	if err := s.Stop(); err != nil {
		log.Error().Err(err).Msg("关闭SMTP服务器时出错")
	}

	log.Info().Msg("已安全关闭服务")
}
