package main

import (
	"context"
	"os"

	"github.com/nats-io/nats.go"
	kmyhconfig "github.com/risy007/kmyh-config"
	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/risy007/kmyh-gateway/agent"
	"github.com/risy007/kmyh-gateway/gateway"
	"github.com/risy007/kmyh-gateway/internal/aibot"
	"github.com/risy007/kmyh-gateway/internal/httphandler"
)

func provideContext() context.Context {
	return context.Background()
}

func main() {
	app := fx.New(
		kmyhconfig.NewConfigModule(),
		kmyhconfig.NewNatsModule(),

		fx.Provide(provideContext),

		aibot.Module,
		agent.Module,
		httphandler.Module,
		gateway.Module,

		fx.Invoke(func(lc fx.Lifecycle, logger *zap.Logger, nc *nats.Conn, httpHandler *httphandler.HttpHandler) {
			lc.Append(fx.Hook{
				OnStart: func(ctx context.Context) error {
					logger.Info("启动 kmyh-gateway HTTP 服务")
					go func() {
						if err := httpHandler.Start(); err != nil {
							logger.Error("HTTP 服务启动失败", zap.Error(err))
						}
					}()
					return nil
				},
				OnStop: func(ctx context.Context) error {
					logger.Info("停止 kmyh-gateway")
					if err := httpHandler.Stop(); err != nil {
						logger.Error("HTTP 服务停止失败", zap.Error(err))
					}
					nc.Close()
					return nil
				},
			})
		}),
	)

	app.Run()

	os.Exit(0)
}
