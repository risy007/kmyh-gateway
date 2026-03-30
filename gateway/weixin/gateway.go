package weixin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/labstack/echo/v4"
	kmyhconfig "github.com/risy007/kmyh-config"
	"github.com/risy007/kmyh-gateway/agent"
	"github.com/risy007/kmyh-gateway/internal/aibot"
	"github.com/risy007/kmyh-gateway/internal/httphandler"
	"github.com/xen0n/go-workwx/v2"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

type (
	WxGateway struct {
		mu            sync.RWMutex
		log           *zap.SugaredLogger
		cfg           kmyhconfig.WeixinConfig
		aibotClient   *aibot.AIBotClient
		agentRegistry *agent.Registry
		channelMgr    *aibot.ChannelManager
		httpHandler   *httphandler.HttpHandler
		routerV1      *echo.Group

		channels map[string]*WxChannel
	}

	WxChannel struct {
		app      *workwx.WorkwxApp
		callback *workwx.HTTPHandler
		config   WxChannelConfig
		cancel   context.CancelFunc
	}

	IncomingWxMessage struct {
		FromUserID string            `json:"from_user_id"`
		MsgType    string            `json:"msg_type"`
		Content    map[string]string `json:"content"`
		MsgID      int64             `json:"msg_id"`
		AgentID    int64             `json:"agent_id,omitempty"`
	}

	WxChannelConfig struct {
		CorpID         string `json:"corp_id"`
		CorpSecret     string `json:"corp_secret"`
		AgentID        int64  `json:"agent_id"`
		Token          string `json:"token"`
		EncodingAESKey string `json:"encoding_aes_key"`
	}

	wxRxHandler struct {
		gateway    *WxGateway
		tenantSlug string
	}
)

type WxGatewayIn struct {
	fx.In
	Logger      *zap.Logger
	AppConfig   *kmyhconfig.AppConfig
	CfgMgr      *kmyhconfig.ConfigManager
	HttpHandler *httphandler.HttpHandler
	ChannelMgr  *aibot.ChannelManager
}

func NewWxGateway(in WxGatewayIn) (*WxGateway, error) {
	log := in.Logger.With(zap.Namespace("[WxGateway]")).Sugar()

	weixinCfgGroup, err := in.CfgMgr.GetGroup("common", in.AppConfig.Env, "weixin")
	if err != nil {
		return nil, err
	}

	var wxConfig kmyhconfig.WeixinConfig
	if err := weixinCfgGroup.Unmarshal(&wxConfig); err != nil {
		log.Warn("解析配置失败", zap.Error(err))
	}

	gw := &WxGateway{
		channelMgr: in.ChannelMgr,
		log:        log,
		cfg:        wxConfig,
		channels:   make(map[string]*WxChannel),
	}

	if in.HttpHandler != nil {
		gw.httpHandler = in.HttpHandler
		gw.routerV1 = in.HttpHandler.RouterV1.Group("/wx")
		gw.registerRoutes()
	}

	weixinCfgGroup.OnChange(func() {
		var newConf kmyhconfig.WeixinConfig
		if err := weixinCfgGroup.Unmarshal(&newConf); err != nil {
			log.Error("解析新配置失败", zap.Error(err))
			return
		}
		gw.mu.Lock()
		gw.cfg = newConf
		gw.mu.Unlock()
	})

	return gw, nil
}

func (gw *WxGateway) registerRoutes() {
	if gw.routerV1 == nil {
		return
	}

	gw.routerV1.POST("/:tenant_slug", gw.webhookHandler)
}

func (gw *WxGateway) webhookHandler(c echo.Context) error {
	tenantSlug := c.Param("tenant_slug")
	if tenantSlug == "" {
		return c.String(http.StatusBadRequest, "tenant_slug is required")
	}

	gw.mu.RLock()
	ch, ok := gw.channels[tenantSlug]
	gw.mu.RUnlock()

	if !ok {
		gw.log.Warn("[WxGateway] 未找到渠道处理器", zap.String("tenant_slug", tenantSlug))
		return c.String(http.StatusNotFound, "channel not found")
	}

	ch.callback.ServeHTTP(c.Response().Writer, c.Request())
	return nil
}

func (gw *WxGateway) SetAIBotClient(client *aibot.AIBotClient) {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	gw.aibotClient = client
}

func (gw *WxGateway) SetAgentRegistry(ar *agent.Registry) {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	gw.agentRegistry = ar
}

func (gw *WxGateway) GetChannelType() string {
	return "weixin"
}

func (gw *WxGateway) InitChannel(channel *aibot.ChannelInfo) error {
	gw.log.Info("[WxGateway] 初始化渠道",
		zap.String("channel_id", channel.ChannelID),
		zap.String("tenant_id", channel.TenantID),
		zap.String("tenant_slug", channel.TenantSlug))

	if channel.Config == nil {
		return nil
	}

	var channelCfg WxChannelConfig
	if err := json.Unmarshal(channel.Config, &channelCfg); err != nil {
		return fmt.Errorf("解析企业微信配置失败: %w", err)
	}

	if channelCfg.CorpID == "" || channelCfg.CorpSecret == "" || channelCfg.AgentID == 0 {
		return fmt.Errorf("corp_id, corp_secret, agent_id 不能为空")
	}

	ctx, cancel := context.WithCancel(context.Background())

	app, err := gw.createWorkwxApp(channelCfg)
	if err != nil {
		cancel()
		return fmt.Errorf("创建 WorkwxApp 失败: %w", err)
	}

	app.SpawnAccessTokenRefresherWithContext(ctx)

	rxHandler := &wxRxHandler{
		gateway:    gw,
		tenantSlug: channel.TenantSlug,
	}

	callback, err := workwx.NewHTTPHandler(channelCfg.Token, channelCfg.EncodingAESKey, rxHandler)
	if err != nil {
		cancel()
		return fmt.Errorf("创建 HTTPHandler 失败: %w", err)
	}

	gw.mu.Lock()
	gw.channels[channel.TenantSlug] = &WxChannel{
		app:      app,
		callback: callback,
		config:   channelCfg,
		cancel:   cancel,
	}
	gw.mu.Unlock()

	gw.log.Info("[WxGateway] 渠道初始化完成",
		zap.String("channel_id", channel.ChannelID),
		zap.String("tenant_slug", channel.TenantSlug))

	return nil
}

func (gw *WxGateway) createWorkwxApp(cfg WxChannelConfig) (*workwx.WorkwxApp, error) {
	if cfg.CorpID == "" {
		return nil, fmt.Errorf("corpid is required")
	}
	if cfg.CorpSecret == "" {
		return nil, fmt.Errorf("corpsecret is required")
	}
	if cfg.AgentID == 0 {
		return nil, fmt.Errorf("agentid is required")
	}

	return workwx.New(cfg.CorpID,
		workwx.WithHTTPClient(http.DefaultClient),
	).WithApp(cfg.CorpSecret, cfg.AgentID), nil
}

func (gw *WxGateway) UpdateChannel(channel *aibot.ChannelInfo) error {
	gw.log.Info("[WxGateway] 更新渠道",
		zap.String("channel_id", channel.ChannelID))

	if err := gw.DeleteChannel(channel.ChannelID); err != nil {
		gw.log.Warn("[WxGateway] 删除旧渠道失败", zap.Error(err))
	}

	return gw.InitChannel(channel)
}

func (gw *WxGateway) DeleteChannel(channelID string) error {
	gw.log.Info("[WxGateway] 删除渠道", zap.String("channel_id", channelID))

	gw.mu.Lock()
	defer gw.mu.Unlock()

	var tenantSlug string
	for slug, ch := range gw.channels {
		_ = ch
		if slug == channelID || fmt.Sprintf("%s-%s", slug, channelID) == channelID {
			tenantSlug = slug
			break
		}
	}

	if tenantSlug == "" {
		for slug := range gw.channels {
			tenantSlug = slug
			break
		}
	}

	if tenantSlug == "" {
		return nil
	}

	if ch, ok := gw.channels[tenantSlug]; ok && ch.cancel != nil {
		ch.cancel()
	}

	delete(gw.channels, tenantSlug)

	return nil
}

func (gw *WxGateway) PauseChannel(channelID string) error {
	gw.log.Info("[WxGateway] 暂停渠道", zap.String("channel_id", channelID))
	return nil
}

func (h *wxRxHandler) OnIncomingMessage(msg *workwx.RxMessage) error {
	gw := h.gateway

	if msg.MsgType == "" || msg.MsgType == workwx.MessageTypeEvent {
		gw.log.Debug("[WxGateway] 忽略事件消息")
		return nil
	}

	incomingMsg := &IncomingWxMessage{
		FromUserID: msg.FromUserID,
		MsgType:    string(msg.MsgType),
		MsgID:      msg.MsgID,
		AgentID:    msg.AgentID,
		Content:    make(map[string]string),
	}

	if msg.MsgType == workwx.MessageTypeText {
		if text, ok := msg.Text(); ok {
			incomingMsg.Content["content"] = text.GetContent()
		}
	}

	return gw.HandleIncomingMessageWithTenant(context.Background(), incomingMsg, h.tenantSlug)
}

func (gw *WxGateway) HandleIncomingMessage(ctx context.Context, msg *IncomingWxMessage, tenantSlug string) error {
	return gw.HandleIncomingMessageWithTenant(ctx, msg, tenantSlug)
}

func (gw *WxGateway) HandleIncomingMessageWithTenant(ctx context.Context, msg *IncomingWxMessage, tenantSlug string) error {
	gw.log.Info("[HandleIncomingMessage] 收到企业微信消息",
		zap.String("tenant_slug", tenantSlug),
		zap.String("from", msg.FromUserID),
		zap.String("msg_type", msg.MsgType))

	if msg.MsgType == "" || msg.MsgType == "event" {
		gw.log.Debug("忽略事件消息")
		return nil
	}

	if gw.channelMgr != nil {
		channel := gw.channelMgr.GetChannelBySlug(tenantSlug)
		if channel != nil && channel.Status != aibot.ChannelStatusActive {
			gw.log.Warn("[HandleIncomingMessage] 渠道已暂停或禁用",
				zap.String("tenant_slug", tenantSlug),
				zap.String("status", string(channel.Status)))
			return gw.sendSuspendedResponse(msg.FromUserID, tenantSlug, channel.Status)
		}
	}

	if msg.MsgType == "" || msg.MsgType == "event" {
		gw.log.Debug("忽略事件消息")
		return nil
	}

	gw.mu.RLock()
	aibotClient := gw.aibotClient
	gw.mu.RUnlock()

	if aibotClient == nil {
		return fmt.Errorf("aibot client not initialized")
	}

	bindInfo, err := aibotClient.GetCustomerBindInfo(ctx, &kmyhconfig.GetCustomerBindInfoRequest{
		Channel:       "weixin",
		ChannelUserID: msg.FromUserID,
	})
	if err != nil {
		gw.log.Error("[HandleIncomingMessage] 获取客户绑定信息失败", zap.Error(err))
		return err
	}

	if !bindInfo.Found || bindInfo.Info == nil {
		gw.log.Warn("[HandleIncomingMessage] 未找到客户绑定信息",
			zap.String("channel_user_id", msg.FromUserID))
		return nil
	}

	gw.mu.RLock()
	agentRegistry := gw.agentRegistry
	gw.mu.RUnlock()

	if agentRegistry == nil {
		return fmt.Errorf("agent registry not initialized")
	}

	agentResp, err := agentRegistry.Process(ctx, &agent.AgentRequest{
		CustomerID:    bindInfo.Info.TenantID,
		Message:       msg,
		Channel:       "weixin",
		ChannelUserID: msg.FromUserID,
		Extra: map[string]interface{}{
			"bind_info": bindInfo.Info,
		},
	})
	if err != nil {
		gw.log.Error("[HandleIncomingMessage] Agent处理失败", zap.Error(err))
		return err
	}

	return gw.sendAgentResponse(msg.FromUserID, agentResp)
}

func (gw *WxGateway) sendAgentResponse(fromUserID string, resp *agent.AgentResponse) error {
	if resp == nil {
		return nil
	}

	gw.mu.RLock()
	var app *workwx.WorkwxApp
	for _, ch := range gw.channels {
		app = ch.app
		break
	}
	gw.mu.RUnlock()

	if app == nil {
		return fmt.Errorf("app not initialized")
	}

	for _, recipient := range resp.Recipients {
		switch resp.Type {
		case "text":
			if err := app.SendTextMessage(&workwx.Recipient{UserIDs: []string{recipient.UserID}}, resp.Content, false); err != nil {
				gw.log.Error("[sendAgentResponse] 发送消息失败", zap.Error(err))
			}
		case "markdown":
			if err := app.SendMarkdownMessage(&workwx.Recipient{UserIDs: []string{recipient.UserID}}, resp.Content, false); err != nil {
				gw.log.Error("[sendAgentResponse] 发送消息失败", zap.Error(err))
			}
		default:
			if err := app.SendTextMessage(&workwx.Recipient{UserIDs: []string{recipient.UserID}}, resp.Content, false); err != nil {
				gw.log.Error("[sendAgentResponse] 发送消息失败", zap.Error(err))
			}
		}
	}

	return nil
}

func (gw *WxGateway) HandleWxCallback(msg *IncomingWxMessage) error {
	return fmt.Errorf("HandleWxCallback is deprecated, use wxRxHandler.OnIncomingMessage instead")
}

func (gw *WxGateway) sendSuspendedResponse(fromUserID, tenantSlug string, status aibot.ChannelStatus) error {
	gw.mu.RLock()
	app := func() *workwx.WorkwxApp {
		if ch, ok := gw.channels[tenantSlug]; ok {
			return ch.app
		}
		for _, ch := range gw.channels {
			return ch.app
		}
		return nil
	}()
	gw.mu.RUnlock()

	if app == nil {
		return fmt.Errorf("app not found for tenant: %s", tenantSlug)
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

	msg := fmt.Sprintf("%s，请联系管理员处理。", reason)
	return app.SendTextMessage(&workwx.Recipient{UserIDs: []string{fromUserID}}, msg, false)
}
