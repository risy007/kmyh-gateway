package agent

import (
	"context"

	"go.uber.org/fx"
	"go.uber.org/zap"
)

var Module = fx.Module("agent",
	fx.Provide(NewGoclawAgent),
	fx.Provide(NewDifyAgent),
	fx.Provide(NewRegistry),
	fx.Invoke(func(lifecycle fx.Lifecycle, logger *zap.Logger, registry *Registry) error {
		lifecycle.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
				logger.Info("[AgentModule] Agent 模块启动完成",
					zap.Strings("types", registry.ListTypes()))
				return nil
			},
		})
		return nil
	}),
)

type Config struct {
	Enabled bool `mapstructure:"enabled"`
}
