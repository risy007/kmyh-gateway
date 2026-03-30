package qqbot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	kmyhconfig "github.com/risy007/kmyh-config"
	"github.com/risy007/kmyh-gateway/agent"
	"github.com/risy007/kmyh-gateway/internal/aibot"
	"github.com/risy007/kmyh-gateway/internal/httphandler"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

type QQBotChannelConfig struct {
	AppID      string `json:"app_id"`
	AppSecret  string `json:"app_secret"`
	Token      string `json:"token"`
	UseSandbox bool   `json:"use_sandbox"`
}

type QQBotGateway struct {
	mu             sync.RWMutex
	log            *zap.SugaredLogger
	cfg            kmyhconfig.QQBotConfig
	aibotClient    *aibot.AIBotClient
	agentRegistry  *agent.Registry
	channelMgr     *aibot.ChannelManager
	httpHandler    *httphandler.HttpHandler
	routerV1       *echo.Group
	channelConfigs map[string]QQBotChannelConfig
}

func NewQQBotGateway(logger *zap.Logger, appConfig *kmyhconfig.AppConfig, cfgMgr *kmyhconfig.ConfigManager, httpHandler *httphandler.HttpHandler, channelMgr *aibot.ChannelManager) (*QQBotGateway, error) {
	log := logger.With(zap.Namespace("[QQBotGateway]")).Sugar()

	qqbotCfgGroup, err := cfgMgr.GetGroup(appConfig.AppName, appConfig.Env, "qqbot")
	if err != nil {
		return nil, err
	}

	var cfg kmyhconfig.QQBotConfig
	if err := qqbotCfgGroup.Unmarshal(&cfg); err != nil {
		log.Warn("解析 qqbot 配置失败", zap.Error(err))
	}

	gw := &QQBotGateway{
		log:            log,
		cfg:            cfg,
		channelMgr:     channelMgr,
		httpHandler:    httpHandler,
		channelConfigs: make(map[string]QQBotChannelConfig),
	}

	if httpHandler != nil {
		gw.routerV1 = httpHandler.RouterV1.Group("/qqbot")
		gw.registerRoutes()
	}

	qqbotCfgGroup.OnChange(func() {
		var newCfg kmyhconfig.QQBotConfig
		if err := qqbotCfgGroup.Unmarshal(&newCfg); err != nil {
			log.Error("解析新 qqbot 配置失败", zap.Error(err))
			return
		}
		gw.mu.Lock()
		gw.cfg = newCfg
		gw.mu.Unlock()
	})

	return gw, nil
}

func (gw *QQBotGateway) registerRoutes() {
	if gw.routerV1 == nil {
		return
	}

	gw.routerV1.POST("/:tenant_slug", gw.webhookHandler)
}

func (gw *QQBotGateway) webhookHandler(c echo.Context) error {
	tenantSlug := c.Param("tenant_slug")
	if tenantSlug == "" {
		return c.String(http.StatusBadRequest, "tenant_slug is required")
	}

	gw.mu.RLock()
	channelCfg, ok := gw.channelConfigs[tenantSlug]
	gw.mu.RUnlock()

	if !ok {
		gw.log.Warn("[QQBotGateway] 未找到渠道配置", zap.String("tenant_slug", tenantSlug))
		return c.String(http.StatusNotFound, "channel not found")
	}

	if err := gw.verifyQQBotRequest(c, channelCfg.Token); err != nil {
		gw.log.Error("[QQBotGateway] 验签失败", zap.Error(err))
		return c.String(http.StatusUnauthorized, "signature verification failed")
	}

	if gw.channelMgr != nil {
		channel := gw.channelMgr.GetChannelBySlug(tenantSlug)
		if channel != nil && channel.Status != aibot.ChannelStatusActive {
			gw.log.Warn("[QQBotGateway] 渠道已暂停或禁用",
				zap.String("tenant_slug", tenantSlug),
				zap.String("status", string(channel.Status)))
			return gw.sendSuspendedResponse(c, tenantSlug, channel.Status)
		}
	}

	var msg map[string]interface{}
	if err := c.Bind(&msg); err != nil {
		gw.log.Error("[QQBotGateway] 解析消息失败", zap.Error(err))
		return c.String(http.StatusBadRequest, "Bad Request")
	}

	msg["tenant_slug"] = tenantSlug

	if err := gw.HandleMessage(msg); err != nil {
		return c.String(http.StatusInternalServerError, err.Error())
	}

	return c.String(http.StatusOK, "success")
}

func (gw *QQBotGateway) verifyQQBotRequest(c echo.Context, token string) error {
	if token == "" {
		return nil
	}

	authHeader := c.Request().Header.Get("Authorization")
	if authHeader == "" {
		return fmt.Errorf("Authorization header is missing")
	}

	authHeader = strings.TrimPrefix(authHeader, "Bearer ")
	if authHeader != token {
		return fmt.Errorf("token mismatch")
	}

	return nil
}

var Module = fx.Module("gateway.qqbot",
	fx.Provide(NewQQBotGateway),
	fx.Invoke(func(cm *aibot.ChannelManager, gw *QQBotGateway) error {
		cm.Register(gw)
		return nil
	}),
)

func (gw *QQBotGateway) SetAIBotClient(client *aibot.AIBotClient) {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	gw.aibotClient = client
}

func (gw *QQBotGateway) SetAgentRegistry(ar *agent.Registry) {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	gw.agentRegistry = ar
}

func (gw *QQBotGateway) Start() error {
	gw.log.Info("[QQBotGateway] 启动")
	return nil
}

func (gw *QQBotGateway) Stop() error {
	gw.log.Info("[QQBotGateway] 停止")
	return nil
}

func (gw *QQBotGateway) HandleMessage(msg interface{}) error {
	gw.log.Info("[QQBotGateway] 收到QQ消息", zap.Any("msg", msg))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	userID := gw.extractUserID(msg)
	if userID == "" {
		gw.log.Warn("[QQBotGateway] 无法提取用户ID")
		return nil
	}

	bindInfo, err := gw.aibotClient.GetCustomerBindInfo(ctx, &kmyhconfig.GetCustomerBindInfoRequest{
		Channel:       "qq",
		ChannelUserID: userID,
	})
	if err != nil {
		gw.log.Error("[QQBotGateway] 获取客户绑定信息失败", zap.Error(err))
		return err
	}

	if !bindInfo.Found || bindInfo.Info == nil {
		gw.log.Warn("[QQBotGateway] 未找到客户绑定信息",
			zap.String("channel_user_id", userID))
		return nil
	}

	_, err = gw.agentRegistry.Process(ctx, &agent.AgentRequest{
		CustomerID:    bindInfo.Info.TenantID,
		Message:       msg,
		Channel:       "qq",
		ChannelUserID: userID,
		Extra: map[string]interface{}{
			"bind_info": bindInfo.Info,
		},
	})
	if err != nil {
		gw.log.Error("[QQBotGateway] Agent处理失败", zap.Error(err))
		return err
	}

	gw.log.Info("[QQBotGateway] 处理完成")
	return nil
}

func (gw *QQBotGateway) sendSuspendedResponse(c echo.Context, tenantSlug string, status aibot.ChannelStatus) error {
	var reason string
	switch status {
	case aibot.ChannelStatusPaused:
		reason = "服务已暂停，请联系管理员处理。"
	case aibot.ChannelStatusDisabled:
		reason = "服务已停用，请联系管理员处理。"
	default:
		reason = "服务暂不可用，请联系管理员处理。"
	}

	return c.String(http.StatusForbidden, reason)
}

func (gw *QQBotGateway) GetChannelType() string {
	return "qqbot"
}

func (gw *QQBotGateway) InitChannel(channel *aibot.ChannelInfo) error {
	gw.log.Info("[QQBotGateway] 初始化渠道",
		zap.String("channel_id", channel.ChannelID),
		zap.String("tenant_id", channel.TenantID),
		zap.String("tenant_slug", channel.TenantSlug))

	if channel.Config == nil {
		return nil
	}

	var cfg kmyhconfig.QQBotConfig
	if err := json.Unmarshal(channel.Config, &cfg); err != nil {
		return fmt.Errorf("解析 QQBot 配置失败: %w", err)
	}

	channelCfg := QQBotChannelConfig{
		AppID:      cfg.AppID,
		AppSecret:  cfg.AppSecret,
		Token:      cfg.Token,
		UseSandbox: cfg.UseSandbox,
	}

	gw.mu.Lock()
	gw.cfg = cfg
	if channel.TenantSlug != "" {
		gw.channelConfigs[channel.TenantSlug] = channelCfg
	}
	gw.mu.Unlock()

	return nil
}

func (gw *QQBotGateway) UpdateChannel(channel *aibot.ChannelInfo) error {
	gw.log.Info("[QQBotGateway] 更新渠道",
		zap.String("channel_id", channel.ChannelID))

	return gw.InitChannel(channel)
}

func (gw *QQBotGateway) DeleteChannel(channelID string) error {
	gw.log.Info("[QQBotGateway] 删除渠道", zap.String("channel_id", channelID))

	gw.mu.Lock()
	defer gw.mu.Unlock()

	gw.cfg = kmyhconfig.QQBotConfig{}
	for slug := range gw.channelConfigs {
		if strings.HasSuffix(slug, channelID) || slug == channelID {
			delete(gw.channelConfigs, slug)
			break
		}
	}

	return nil
}

func (gw *QQBotGateway) PauseChannel(channelID string) error {
	gw.log.Info("[QQBotGateway] 暂停渠道", zap.String("channel_id", channelID))
	return nil
}

func (gw *QQBotGateway) extractUserID(msg interface{}) string {
	data, err := json.Marshal(msg)
	if err != nil {
		return ""
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return ""
	}

	if author, ok := raw["author"].(map[string]interface{}); ok {
		if id, ok := author["id"].(string); ok {
			return id
		}
	}

	if userID, ok := raw["user_id"].(string); ok {
		return userID
	}

	if id, ok := raw["id"].(string); ok {
		return id
	}

	return ""
}
