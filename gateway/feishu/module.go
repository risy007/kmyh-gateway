package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	kmyhconfig "github.com/risy007/kmyh-config"
	"github.com/risy007/kmyh-gateway/agent"
	"github.com/risy007/kmyh-gateway/internal/aibot"
	"github.com/risy007/kmyh-gateway/internal/httphandler"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

type (
	FeishuGateway struct {
		mu            sync.RWMutex
		log           *zap.SugaredLogger
		cfg           kmyhconfig.FeishuConfig
		aibotClient   *aibot.AIBotClient
		agentRegistry *agent.Registry
		channelMgr    *aibot.ChannelManager
		httpHandler   *httphandler.HttpHandler
		routerV1      *echo.Group

		channels map[string]*FeishuChannel
	}

	FeishuChannel struct {
		client     *LarkClient
		dispatcher *dispatcher.EventDispatcher
		config     FeishuChannelConfig
	}

	LarkClient struct {
		appID     string
		appSecret string
	}

	FeishuChannelConfig struct {
		AppID             string
		AppSecret         string
		CallbackType      string
		EncryptKey        string
		VerificationToken string
	}
)

func NewFeishuGateway(logger *zap.Logger, appConfig *kmyhconfig.AppConfig, cfgMgr *kmyhconfig.ConfigManager, httpHandler *httphandler.HttpHandler, channelMgr *aibot.ChannelManager) (*FeishuGateway, error) {
	log := logger.With(zap.Namespace("[FeishuGateway]")).Sugar()

	feishuCfgGroup, err := cfgMgr.GetGroup(appConfig.AppName, appConfig.Env, "feishu")
	if err != nil {
		return nil, err
	}

	var cfg kmyhconfig.FeishuConfig
	if err := feishuCfgGroup.Unmarshal(&cfg); err != nil {
		log.Warn("解析 feishu 配置失败", zap.Error(err))
	}

	gw := &FeishuGateway{
		log:        log,
		cfg:        cfg,
		channelMgr: channelMgr,
		channels:   make(map[string]*FeishuChannel),
	}

	if httpHandler != nil {
		gw.httpHandler = httpHandler
		gw.routerV1 = httpHandler.RouterV1.Group("/feishu")
		gw.registerRoutes()
	}

	feishuCfgGroup.OnChange(func() {
		var newCfg kmyhconfig.FeishuConfig
		if err := feishuCfgGroup.Unmarshal(&newCfg); err != nil {
			log.Error("解析新 feishu 配置失败", zap.Error(err))
			return
		}
		gw.mu.Lock()
		gw.cfg = newCfg
		gw.mu.Unlock()
	})

	return gw, nil
}

func (gw *FeishuGateway) registerRoutes() {
	if gw.routerV1 == nil {
		return
	}

	gw.routerV1.POST("/:tenant_slug", gw.webhookHandler)
}

func (gw *FeishuGateway) webhookHandler(c echo.Context) error {
	tenantSlug := c.Param("tenant_slug")
	if tenantSlug == "" {
		return c.String(http.StatusBadRequest, "tenant_slug is required")
	}

	gw.mu.RLock()
	ch, ok := gw.channels[tenantSlug]
	gw.mu.RUnlock()

	if !ok {
		gw.log.Warn("[FeishuGateway] 未找到渠道处理器", zap.String("tenant_slug", tenantSlug))
		return c.String(http.StatusNotFound, "channel not found")
	}

	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return c.String(http.StatusBadRequest, "Bad Request")
	}

	req := &larkevent.EventReq{
		Header:     c.Request().Header,
		Body:       body,
		RequestURI: c.Request().URL.Path,
	}

	if err := ch.dispatcher.VerifySign(context.Background(), req); err != nil {
		gw.log.Error("[FeishuGateway] 验签失败", zap.Error(err))
		return c.String(http.StatusUnauthorized, "signature verification failed")
	}

	resp := ch.dispatcher.Handle(context.Background(), req)
	if resp == nil {
		return c.String(http.StatusOK, "success")
	}

	return c.Blob(resp.StatusCode, "", resp.Body)
}

var Module = fx.Module("gateway.feishu",
	fx.Provide(NewFeishuGateway),
	fx.Invoke(func(cm *aibot.ChannelManager, gw *FeishuGateway) error {
		cm.Register(gw)
		return nil
	}),
)

func (gw *FeishuGateway) SetAIBotClient(client *aibot.AIBotClient) {
	gw.aibotClient = client
}

func (gw *FeishuGateway) SetAgentRegistry(ar *agent.Registry) {
	gw.agentRegistry = ar
}

func (gw *FeishuGateway) Start() error {
	gw.log.Info("[FeishuGateway] 启动")
	return nil
}

func (gw *FeishuGateway) Stop() error {
	gw.log.Info("[FeishuGateway] 停止")
	return nil
}

func (gw *FeishuGateway) GetChannelType() string {
	return "feishu"
}

func (gw *FeishuGateway) InitChannel(channel *aibot.ChannelInfo) error {
	gw.log.Info("[FeishuGateway] 初始化渠道",
		zap.String("channel_id", channel.ChannelID),
		zap.String("tenant_id", channel.TenantID),
		zap.String("tenant_slug", channel.TenantSlug))

	if channel.Config == nil {
		return nil
	}

	var rawCfg struct {
		AppID             string `json:"app_id"`
		AppSecret         string `json:"app_secret"`
		CallbackType      string `json:"callback_type"`
		EncryptKey        string `json:"encrypt_key,omitempty"`
		VerificationToken string `json:"verification_token,omitempty"`
		WebhookPath       string `json:"webhook_path,omitempty"`
	}

	if err := json.Unmarshal(channel.Config, &rawCfg); err != nil {
		return fmt.Errorf("解析飞书配置失败: %w", err)
	}

	if rawCfg.AppID == "" || rawCfg.AppSecret == "" {
		return fmt.Errorf("app_id or app_secret is empty")
	}

	channelCfg := FeishuChannelConfig{
		AppID:             rawCfg.AppID,
		AppSecret:         rawCfg.AppSecret,
		CallbackType:      rawCfg.CallbackType,
		EncryptKey:        rawCfg.EncryptKey,
		VerificationToken: rawCfg.VerificationToken,
	}

	dp := dispatcher.NewEventDispatcher(rawCfg.EncryptKey, rawCfg.VerificationToken)

	dp.OnP2MessageReceiveV1(func(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
		return gw.handleMessageReceive(ctx, channel.TenantSlug, event)
	})

	gw.mu.Lock()
	gw.channels[channel.TenantSlug] = &FeishuChannel{
		client: &LarkClient{
			appID:     rawCfg.AppID,
			appSecret: rawCfg.AppSecret,
		},
		dispatcher: dp,
		config:     channelCfg,
	}
	gw.mu.Unlock()

	gw.log.Info("[FeishuGateway] 渠道初始化完成",
		zap.String("channel_id", channel.ChannelID),
		zap.String("tenant_slug", channel.TenantSlug))

	return nil
}

func (gw *FeishuGateway) handleMessageReceive(ctx context.Context, tenantSlug string, event *larkim.P2MessageReceiveV1) error {
	if event == nil || event.Event == nil || event.Event.Message == nil {
		return nil
	}

	msg := event.Event.Message
	sender := event.Event.Sender

	gw.log.Info("[FeishuGateway] 收到飞书消息",
		zap.String("tenant_slug", tenantSlug))

	var userID string
	var msgID, chatType, msgType string
	if msg.MessageId != nil {
		msgID = *msg.MessageId
	}
	if msg.ChatType != nil {
		chatType = *msg.ChatType
	}
	if msg.MessageType != nil {
		msgType = *msg.MessageType
	}
	if sender != nil && sender.SenderId != nil && sender.SenderId.OpenId != nil {
		userID = *sender.SenderId.OpenId
	}

	gw.log.Info("[FeishuGateway] 收到飞书消息",
		zap.String("tenant_slug", tenantSlug),
		zap.String("msg_id", msgID),
		zap.String("chat_type", chatType),
		zap.String("msg_type", msgType))

	if gw.channelMgr != nil {
		channel := gw.channelMgr.GetChannelBySlug(tenantSlug)
		if channel != nil && channel.Status != aibot.ChannelStatusActive {
			gw.log.Warn("[FeishuGateway] 渠道已暂停或禁用",
				zap.String("tenant_slug", tenantSlug),
				zap.String("status", string(channel.Status)))
			return gw.sendSuspendedResponse(userID, tenantSlug, channel.Status)
		}
	}

	if userID == "" {
		gw.log.Warn("[FeishuGateway] 无法提取用户ID")
		return nil
	}

	bindInfo, err := gw.aibotClient.GetCustomerBindInfo(ctx, &kmyhconfig.GetCustomerBindInfoRequest{
		Channel:       "feishu",
		ChannelUserID: userID,
	})
	if err != nil {
		gw.log.Error("[FeishuGateway] 获取客户绑定信息失败", zap.Error(err))
		return err
	}

	if !bindInfo.Found || bindInfo.Info == nil {
		gw.log.Warn("[FeishuGateway] 未找到客户绑定信息",
			zap.String("channel_user_id", userID))
		return nil
	}

	messageContent := ""
	if msg.Content != nil {
		if contentBytes, err := json.Marshal(msg.Content); err == nil {
			messageContent = string(contentBytes)
		}
	}

	agentMsg := map[string]interface{}{
		"tenant_slug": tenantSlug,
		"msg_id":      msg.MessageId,
		"msg_type":    msg.MessageType,
		"chat_type":   msg.ChatType,
		"user_id":     userID,
		"content":     messageContent,
	}

	_, err = gw.agentRegistry.Process(ctx, &agent.AgentRequest{
		CustomerID:    bindInfo.Info.TenantID,
		Message:       agentMsg,
		Channel:       "feishu",
		ChannelUserID: userID,
		Extra: map[string]interface{}{
			"bind_info": bindInfo.Info,
		},
	})
	if err != nil {
		gw.log.Error("[FeishuGateway] Agent处理失败", zap.Error(err))
		return err
	}

	return nil
}

func (gw *FeishuGateway) UpdateChannel(channel *aibot.ChannelInfo) error {
	gw.log.Info("[FeishuGateway] 更新渠道",
		zap.String("channel_id", channel.ChannelID))

	if err := gw.DeleteChannel(channel.ChannelID); err != nil {
		gw.log.Warn("[FeishuGateway] 删除旧渠道失败", zap.Error(err))
	}

	return gw.InitChannel(channel)
}

func (gw *FeishuGateway) DeleteChannel(channelID string) error {
	gw.log.Info("[FeishuGateway] 删除渠道", zap.String("channel_id", channelID))

	gw.mu.Lock()
	defer gw.mu.Unlock()

	var tenantSlug string
	for slug, ch := range gw.channels {
		_ = ch
		if slug == channelID || strings.HasSuffix(slug, channelID) {
			tenantSlug = slug
			break
		}
	}

	if tenantSlug != "" {
		delete(gw.channels, tenantSlug)
	}

	return nil
}

func (gw *FeishuGateway) PauseChannel(channelID string) error {
	gw.log.Info("[FeishuGateway] 暂停渠道", zap.String("channel_id", channelID))
	return nil
}

func (gw *FeishuGateway) HandleMessage(msg interface{}) error {
	gw.log.Info("[FeishuGateway] 收到飞书消息", zap.Any("msg", msg))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	userID := gw.extractUserID(msg)
	if userID == "" {
		gw.log.Warn("[FeishuGateway] 无法提取用户ID")
		return nil
	}

	bindInfo, err := gw.aibotClient.GetCustomerBindInfo(ctx, &kmyhconfig.GetCustomerBindInfoRequest{
		Channel:       "feishu",
		ChannelUserID: userID,
	})
	if err != nil {
		gw.log.Error("[FeishuGateway] 获取客户绑定信息失败", zap.Error(err))
		return err
	}

	if !bindInfo.Found || bindInfo.Info == nil {
		gw.log.Warn("[FeishuGateway] 未找到客户绑定信息",
			zap.String("channel_user_id", userID))
		return nil
	}

	_, err = gw.agentRegistry.Process(ctx, &agent.AgentRequest{
		CustomerID:    bindInfo.Info.TenantID,
		Message:       msg,
		Channel:       "feishu",
		ChannelUserID: userID,
		Extra: map[string]interface{}{
			"bind_info": bindInfo.Info,
		},
	})
	if err != nil {
		gw.log.Error("[FeishuGateway] Agent处理失败", zap.Error(err))
		return err
	}

	gw.log.Info("[FeishuGateway] 处理完成")
	return nil
}

func (gw *FeishuGateway) GetCallbackType() string {
	gw.mu.RLock()
	defer gw.mu.RUnlock()
	return ""
}

func (gw *FeishuGateway) sendSuspendedResponse(userID, tenantSlug string, status aibot.ChannelStatus) error {
	gw.mu.RLock()
	ch, ok := gw.channels[tenantSlug]
	gw.mu.RUnlock()

	if !ok || ch == nil || ch.client == nil {
		gw.log.Warn("[FeishuGateway] 无法发送暂停提示，client未找到",
			zap.String("tenant_slug", tenantSlug))
		return nil
	}

	var reason string
	switch status {
	case aibot.ChannelStatusPaused:
		reason = "服务已暂停"
	case aibot.ChannelStatusDisabled:
		reason = "服务已停用"
	default:
		reason = "服务暂不可用"
	}

	gw.log.Info("[FeishuGateway] 发送暂停提示",
		zap.String("tenant_slug", tenantSlug),
		zap.String("user_id", userID),
		zap.String("reason", reason))

	return nil
}

func (gw *FeishuGateway) extractUserID(msg interface{}) string {
	data, err := json.Marshal(msg)
	if err != nil {
		return ""
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return ""
	}

	if sender, ok := raw["sender"].(map[string]interface{}); ok {
		if id, ok := sender["sender_id"].(map[string]interface{}); ok {
			if openID, ok := id["open_id"].(string); ok {
				return openID
			}
		}
	}

	if userID, ok := raw["user_id"].(string); ok {
		return userID
	}

	return ""
}
