package aibot

import (
	"context"

	"go.uber.org/fx"
)

var Module = fx.Module("aibot",
	fx.Provide(NewAIBotClient),
	fx.Provide(NewGatewayServer),
	fx.Provide(NewChannelManager),
	fx.Invoke(func(lc fx.Lifecycle, cm *ChannelManager, gs *GatewayServer, client *AIBotClient) error {
		lc.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
				return gs.Start()
			},
			OnStop: func(ctx context.Context) error {
				return gs.Stop()
			},
		})
		return nil
	}),
)

func RegisterChannel(cm *ChannelManager, handler ChannelHandler) {
	cm.Register(handler)
}
