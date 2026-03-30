package main

import (
	"context"
	"os"

	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/risy007/kmyh-gateway/gateway"
	"github.com/risy007/kmyh-gateway/internal/aibot"
	"github.com/risy007/kmyh-gateway/internal/goclaw"
)

func main() {
	logger := zap.NewExample()
	defer logger.Sync()

	app := fx.New(
		fx.Provide(
			fx.Annotate(
				func() *zap.Logger { return logger },
				fx.ResultTags(`group:"logger"`),
			),
			aibot.NewChannelManager,
			goclaw.NewGoclawService,
			goclaw.NewGoclawAgentProcessor,
		),
		gateway.Module,
		fx.Invoke(func(lc fx.Lifecycle, cm *aibot.ChannelManager) error {
			lc.Append(fx.Hook{
				OnStart: func(ctx context.Context) error {
					logger.Info("启动 kmyh-gateway")
					return nil
				},
				OnStop: func(ctx context.Context) error {
					logger.Info("停止 kmyh-gateway")
					return nil
				},
			})
			return nil
		}),
	)

	if err := app.Start(context.Background()); err != nil {
		logger.Fatal("启动失败", zap.Error(err))
	}

	<-app.Done()

	if err := app.Stop(context.Background()); err != nil {
		logger.Fatal("停止失败", zap.Error(err))
	}

	os.Exit(0)
}
