package httphandler

import (
	"go.uber.org/fx"
)

var Module = fx.Options(
	fx.Provide(NewHttpHandler),
)
