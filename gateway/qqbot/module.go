package qqbot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
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

type qqbotChannelState struct {
	config      QQBotChannelConfig
	mu          sync.RWMutex
	accessToken string
	tokenExpire time.Time
}

type QQBotGateway struct {
	mu            sync.RWMutex
	log           *zap.SugaredLogger
	cfg           kmyhconfig.QQBotConfig
	aibotClient   *aibot.AIBotClient
	agentRegistry *agent.Registry
	channelMgr    *aibot.ChannelManager
	httpHandler   *httphandler.HttpHandler
	routerV1      *echo.Group
	httpClient    *http.Client
	channelStates map[string]*qqbotChannelState
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
		log:           log,
		cfg:           cfg,
		channelMgr:    channelMgr,
		httpHandler:   httpHandler,
		httpClient:    &http.Client{Timeout: 15 * time.Second},
		channelStates: make(map[string]*qqbotChannelState),
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
	state, ok := gw.channelStates[tenantSlug]
	gw.mu.RUnlock()

	if !ok || state == nil {
		gw.log.Warn("[QQBotGateway] 未找到渠道配置", zap.String("tenant_slug", tenantSlug))
		return c.String(http.StatusNotFound, "channel not found")
	}

	if err := gw.verifyQQBotRequest(c, state.config.Token); err != nil {
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
	fx.Invoke(func(cm *aibot.ChannelManager, gw *QQBotGateway, aibotClient *aibot.AIBotClient, ar *agent.Registry) error {
		gw.SetAIBotClient(aibotClient)
		gw.SetAgentRegistry(ar)
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

	mc := gw.extractMessageContext(msg)

	bindInfo, err := gw.aibotClient.GetCustomerBindInfo(ctx, &kmyhconfig.GetCustomerBindInfoRequest{
		Channel:       "qq",
		ChannelUserID: userID,
		TenantSlug:    mc.TenantSlug,
	})
	if err != nil {
		gw.log.Error("[QQBotGateway] 获取客户绑定信息失败", zap.Error(err))
		return err
	}

	if !bindInfo.Found || bindInfo.Info == nil || bindInfo.Info.UnifiedUserID == "" {
		gw.log.Warn("[QQBotGateway] 未找到客户绑定信息或尚未关联统一用户",
			zap.String("channel_user_id", userID))
		return gw.sendTextMessage(ctx, msg, "您的账号尚未与系统用户关联，请联系管理员在租户详情页完成统一用户绑定后再使用。")
	}

	agentResp, err := gw.agentRegistry.Process(ctx, &agent.AgentRequest{
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
	return gw.sendAgentResponse(ctx, msg, agentResp)
}

func (gw *QQBotGateway) sendTextMessage(ctx context.Context, msg interface{}, text string) error {
	mc := gw.extractMessageContext(msg)
	tenantSlug := mc.TenantSlug
	if tenantSlug == "" {
		gw.log.Warn("[QQBotGateway] 无法发送文本消息，缺少tenant_slug")
		return nil
	}

	gw.mu.RLock()
	state, ok := gw.channelStates[tenantSlug]
	gw.mu.RUnlock()

	if !ok || state == nil {
		gw.log.Warn("[QQBotGateway] 无法发送文本消息，渠道未找到",
			zap.String("tenant_slug", tenantSlug))
		return nil
	}

	token, err := gw.getAccessToken(ctx, state)
	if err != nil {
		gw.log.Error("[QQBotGateway] 获取access_token失败", zap.Error(err))
		return err
	}

	baseURL := "https://api.sgroup.qq.com"
	if state.config.UseSandbox {
		baseURL = "https://sandbox.api.sgroup.qq.com"
	}

	var endpoint string
	switch mc.MsgType {
	case "group", "GROUP_AT_MESSAGE_CREATE":
		if mc.GroupID != "" {
			endpoint = fmt.Sprintf("%s/v2/groups/%s/messages", baseURL, mc.GroupID)
		}
	case "guild", "CHANNEL_MESSAGE_CREATE", "AT_MESSAGE_CREATE":
		if mc.ChannelID != "" {
			endpoint = fmt.Sprintf("%s/channels/%s/messages", baseURL, mc.ChannelID)
		}
	default:
		if mc.UserID != "" {
			endpoint = fmt.Sprintf("%s/v2/users/%s/messages", baseURL, mc.UserID)
		}
	}

	if endpoint == "" {
		gw.log.Warn("[QQBotGateway] 无法确定消息发送端点")
		return nil
	}

	payload := map[string]interface{}{
		"msg_type": 0,
		"content":  text,
		"msg_seq":  rand.Intn(1000000),
	}
	if mc.MsgID != "" {
		payload["msg_id"] = mc.MsgID
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "QQBot "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := gw.httpClient.Do(req)
	if err != nil {
		gw.log.Error("[QQBotGateway] 发送文本消息失败", zap.Error(err))
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		gw.log.Error("[QQBotGateway] 发送文本消息返回非成功状态码",
			zap.Int("status", resp.StatusCode),
			zap.String("body", string(respBody)))
		return fmt.Errorf("qqbot send message failed: %d", resp.StatusCode)
	}
	return nil
}

func (gw *QQBotGateway) sendAgentResponse(ctx context.Context, msg interface{}, resp *agent.AgentResponse) error {
	if resp == nil || resp.Content == "" {
		return nil
	}
	return gw.sendTextMessage(ctx, msg, resp.Content)
}

func (gw *QQBotGateway) getAccessToken(ctx context.Context, state *qqbotChannelState) (string, error) {
	state.mu.RLock()
	token := state.accessToken
	expire := state.tokenExpire
	state.mu.RUnlock()

	if token != "" && time.Now().Before(expire.Add(-5*time.Minute)) {
		return token, nil
	}

	reqBody, _ := json.Marshal(map[string]string{
		"appId":        state.config.AppID,
		"clientSecret": state.config.AppSecret,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://bots.qq.com/app/getAppAccessToken", bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := gw.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	state.mu.Lock()
	state.accessToken = result.AccessToken
	state.tokenExpire = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
	state.mu.Unlock()

	return result.AccessToken, nil
}

type qqMessageContext struct {
	TenantSlug string
	UserID     string
	GroupID    string
	ChannelID  string
	GuildID    string
	MsgID      string
	MsgType    string
}

func (gw *QQBotGateway) extractMessageContext(msg interface{}) qqMessageContext {
	var mc qqMessageContext

	data, err := json.Marshal(msg)
	if err != nil {
		return mc
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return mc
	}

	if ts, ok := raw["tenant_slug"].(string); ok {
		mc.TenantSlug = ts
	}

	var d map[string]interface{}
	if v, ok := raw["d"].(map[string]interface{}); ok {
		d = v
	}

	getString := func(m map[string]interface{}, key string) string {
		if v, ok := m[key].(string); ok {
			return v
		}
		return ""
	}

	mc.MsgType = getString(raw, "t")
	if mc.MsgType == "" {
		mc.MsgType = getString(raw, "msg_type")
	}

	mc.MsgID = getString(raw, "id")
	if d != nil && mc.MsgID == "" {
		mc.MsgID = getString(d, "id")
	}

	mc.GroupID = getString(raw, "group_id")
	if d != nil && mc.GroupID == "" {
		mc.GroupID = getString(d, "group_id")
	}
	if d != nil && mc.GroupID == "" {
		mc.GroupID = getString(d, "group_openid")
	}

	mc.ChannelID = getString(raw, "channel_id")
	if d != nil && mc.ChannelID == "" {
		mc.ChannelID = getString(d, "channel_id")
	}

	mc.GuildID = getString(raw, "guild_id")
	if d != nil && mc.GuildID == "" {
		mc.GuildID = getString(d, "guild_id")
	}

	mc.UserID = gw.extractUserID(msg)

	return mc
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
		gw.channelStates[channel.TenantSlug] = &qqbotChannelState{
			config: channelCfg,
		}
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
	for slug := range gw.channelStates {
		if strings.HasSuffix(slug, channelID) || slug == channelID {
			delete(gw.channelStates, slug)
			break
		}
	}

	return nil
}

func (gw *QQBotGateway) PauseChannel(channelID string) error {
	gw.log.Info("[QQBotGateway] 暂停渠道", zap.String("channel_id", channelID))

	gw.mu.Lock()
	defer gw.mu.Unlock()

	if state, ok := gw.channelStates[channelID]; ok {
		state.mu.Lock()
		state.accessToken = ""
		state.tokenExpire = time.Time{}
		state.mu.Unlock()
	}

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
