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
				if err := gs.Start(); err != nil {
					return err
				}

				if err := gs.SubscribeMigrationEvents(); err != nil {
					return err
				}

				channels, err := client.ListChannels(ctx, "", "")
				if err != nil {
					return err
				}

				if err := cm.InitChannels(channels); err != nil {
					return err
				}

				return nil
			},
		})
		return nil
	}),
)

func RegisterChannel(cm *ChannelManager, handler ChannelHandler) {
	cm.Register(handler)
}
