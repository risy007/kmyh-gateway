package httphandler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/go-playground/validator/v10"
	"github.com/labstack/echo/v4"
	"github.com/labstack/gommon/log"
	"github.com/risy007/kmyh-config"
	"github.com/risy007/kmyh-gateway/utils/echox"
	"github.com/risy007/kmyh-gateway/utils/slice"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"net/http"
	"reflect"
	"strings"
	"sync"
)

type (
	inHttpHandlerParam struct {
		fx.In
		Logger    *zap.Logger
		AppConfig *config.AppConfig
		CfgMgr    *config.ConfigManager
	}

	HttpHandler struct {
		mu       sync.RWMutex
		Engine   *echo.Echo
		RouterV1 *echo.Group
		Config   *config.HttpConfig
		Validate *validator.Validate
	}
	Validator struct {
		validate *validator.Validate
	}

	// Implement the bind method to verify the request's struct for parameter validation
	BinderWithValidation struct{}
)

// NewHttpHandler creates a new request handler
func NewHttpHandler(in inHttpHandlerParam) (*HttpHandler, error) {
	CfgGroup, err := in.CfgMgr.GetGroup(in.AppConfig.AppName, in.AppConfig.Env, "http")
	if err != nil {
		return nil, fmt.Errorf("获取 HTTP 配置组失败: %w", err)
	}

	var conf config.HttpConfig
	if err := CfgGroup.Unmarshal(&conf); err != nil {
		log.Warn("解析配置失败", zap.Error(err))
	}

	// Error handlers
	echo.NotFoundHandler = func(ctx echo.Context) error {
		return echox.Response{Code: http.StatusNotFound}.JSON(ctx)
	}
	echo.MethodNotAllowedHandler = func(ctx echo.Context) error {
		return echox.Response{Code: http.StatusMethodNotAllowed}.JSON(ctx)
	}
	// new engine
	engine := echo.New()
	engine.HidePort = true
	engine.HideBanner = true
	engine.Binder = &BinderWithValidation{}
	// set http handler
	httpHandler := &HttpHandler{
		Engine:   engine,
		RouterV1: engine.Group(conf.Prefix),
		Config:   &conf,
	}

	// 监听配置变更
	CfgGroup.OnChange(func() {
		in.Logger.Info("HTTP 配置已更新 (注意：部分变更如端口和路由前缀需重启生效)")
		var newConf config.HttpConfig
		if err := CfgGroup.Unmarshal(&newConf); err != nil {
			in.Logger.Error("解析新 HTTP 配置失败", zap.Error(err))
			return
		}
		httpHandler.mu.Lock()
		httpHandler.Config = &newConf
		httpHandler.mu.Unlock()
	})

	// custom the error handler
	httpHandler.Engine.HTTPErrorHandler = func(err error, ctx echo.Context) {
		var (
			code    = http.StatusInternalServerError
			message interface{}
		)

		he, ok := err.(*echo.HTTPError)
		if ok {
			code = he.Code
			message = he.Message

			if he.Internal != nil {
				message = fmt.Errorf("%v - %v", message, he.Internal)
			}
		}

		// Send response
		if !ctx.Response().Committed {
			// https://www.w3.org/Protocols/rfc2616/rfc2616-sec9.html
			if ctx.Request().Method == http.MethodHead {
				err = ctx.NoContent(he.Code)
			} else {
				err = echox.Response{
					Code:    code,
					Message: message,
				}.JSON(ctx)
			}

			if err != nil {
				in.Logger.Error(err.Error())
			}
		}
	}

	// override the default validator
	httpHandler.Engine.Validator = func() echo.Validator {
		v := validator.New()

		v.RegisterValidation("json", func(fl validator.FieldLevel) bool {
			var js json.RawMessage
			return json.Unmarshal([]byte(fl.Field().String()), &js) == nil
		})

		v.RegisterValidation("in", func(fl validator.FieldLevel) bool {
			value := fl.Field().String()
			if slice.ContainsString(strings.Split(fl.Param(), ";"), value) || value == "" {
				return true
			}

			return false
		})

		return &Validator{validate: v}
	}()

	return httpHandler, nil
}

func (s *HttpHandler) Start() error {
	s.mu.RLock()
	addr := s.Config.ListenAddr()
	s.mu.RUnlock()
	return s.Engine.Start(addr)
}

func (s *HttpHandler) Stop() error {
	return s.Engine.Close()
}
func (BinderWithValidation) Bind(i interface{}, ctx echo.Context) error {
	binder := &echo.DefaultBinder{}

	if err := binder.Bind(i, ctx); err != nil {
		if he, ok := err.(*echo.HTTPError); ok {
			if msg, ok := he.Message.(string); ok {
				return errors.New(msg)
			}
		}
		return err
	}

	if err := ctx.Validate(i); err != nil {
		// Validate only provides verification function for struct.
		// When the requested data type is not struct,
		// the variable should be considered legal after the bind succeeds.
		v := reflect.ValueOf(i)
		if v.Kind() == reflect.Ptr {
			v = v.Elem()
		}
		if v.Kind() != reflect.Struct {
			return nil
		}

		var buf bytes.Buffer
		if ferrs, ok := err.(validator.ValidationErrors); ok {
			for _, ferr := range ferrs {
				buf.WriteString("Validation failed on ")
				buf.WriteString(ferr.Tag())
				buf.WriteString(" for ")
				buf.WriteString(ferr.StructField())
				buf.WriteString("\n")
			}

			return errors.New(buf.String())
		}

		return err
	}

	return nil
}

func (a *Validator) Validate(i interface{}) error {
	return a.validate.Struct(i)
}
