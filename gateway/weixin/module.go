package weixin

import (
	"context"

	"github.com/risy007/kmyh-gateway/agent"
	"github.com/risy007/kmyh-gateway/internal/aibot"
	"go.uber.org/fx"
)

var Module = fx.Module("gateway.weixin",
	fx.Provide(NewWxGateway),
	fx.Invoke(func(cm *aibot.ChannelManager, gw *WxGateway) error {
		cm.Register(gw)
		return nil
	}),
	fx.Invoke(func(lc fx.Lifecycle, gw *WxGateway, aibotClient *aibot.AIBotClient, ar *agent.Registry) error {
		gw.SetAIBotClient(aibotClient)
		gw.SetAgentRegistry(ar)

		lc.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
				return nil
			},
		})
		return nil
	}),
)
