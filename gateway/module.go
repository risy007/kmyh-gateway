package gateway

import (
	feishu "github.com/risy007/kmyh-gateway/gateway/feishu"
	qqbot "github.com/risy007/kmyh-gateway/gateway/qqbot"
	weixin "github.com/risy007/kmyh-gateway/gateway/weixin"

	"go.uber.org/fx"
)

var Module = fx.Module("gateway",
	fx.Options(
		weixin.Module,
		feishu.Module,
		qqbot.Module,
	),
)
