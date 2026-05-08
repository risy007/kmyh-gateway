package aibot

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/risy007/kmyh-gateway/internal/httphandler"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

type GatewayServer struct {
	log         *zap.Logger
	ctx         context.Context
	nc          *nats.Conn
	manager     *ChannelManager
	httpHandler *httphandler.HttpHandler
	aibotClient *AIBotClient
	subs        []*nats.Subscription
}

type serverInParams struct {
	fx.In
	Logger      *zap.Logger
	Ctx         context.Context
	NC          *nats.Conn
	Manager     *ChannelManager
	HttpHandler *httphandler.HttpHandler
	AIBotClient *AIBotClient
}

func NewGatewayServer(in serverInParams) (*GatewayServer, error) {
	log := in.Logger.With(zap.Namespace("[GatewayServer]"))

	server := &GatewayServer{
		log:         log,
		ctx:         in.Ctx,
		nc:          in.NC,
		manager:     in.Manager,
		httpHandler: in.HttpHandler,
		aibotClient: in.AIBotClient,
	}

	log.Info("GatewayServer 初始化完成")
	return server, nil
}

func (s *GatewayServer) Start() error {
	s.log.Info("启动 GatewayServer")

	if s.nc == nil {
		return fmt.Errorf("NATS 连接未初始化")
	}

	if err := s.subscribeChannelEvents(); err != nil {
		s.log.Error("订阅渠道事件失败", zap.Error(err))
		return err
	}

	if err := s.subscribePeerSyncEvents(); err != nil {
		s.log.Error("订阅同步事件失败", zap.Error(err))
		return err
	}

	if err := s.subscribeMigrationEvents(); err != nil {
		s.log.Error("订阅迁移事件失败", zap.Error(err))
		return err
	}

	if s.aibotClient != nil {
		if err := s.InitChannelsFromAdmin(s.ctx, s.aibotClient); err != nil {
			s.log.Warn("从 Admin 初始化渠道失败", zap.Error(err))
		}
	}

	return nil
}

func (s *GatewayServer) Stop() error {
	s.log.Info("停止 GatewayServer")
	for _, sub := range s.subs {
		if sub != nil {
			sub.Unsubscribe()
		}
	}
	return nil
}

func (s *GatewayServer) subscribeChannelEvents() error {
	channels := []struct {
		subject string
		handler nats.MsgHandler
	}{
		{"gateway.channels.create", s.onChannelCreate},
		{"gateway.channels.update", s.onChannelUpdate},
		{"gateway.channels.destroy", s.onChannelDestroy},
		{"gateway.channels.pause", s.onChannelPause},
		{"gateway.channels.resume", s.onChannelResume},
	}

	for _, ch := range channels {
		sub, err := s.nc.Subscribe(ch.subject, ch.handler)
		if err != nil {
			return fmt.Errorf("订阅 %s 失败: %w", ch.subject, err)
		}
		s.subs = append(s.subs, sub)
	}

	s.log.Info("订阅渠道事件成功")
	return nil
}

func (s *GatewayServer) subscribePeerSyncEvents() error {
	channels := []struct {
		subject string
		handler nats.MsgHandler
	}{
		{"gateway.channels.sync.create", s.onSyncCreate},
		{"gateway.channels.sync.update", s.onSyncUpdate},
		{"gateway.channels.sync.destroy", s.onSyncDestroy},
		{"gateway.channels.sync.pause", s.onSyncPause},
		{"gateway.channels.sync.resume", s.onSyncResume},
	}

	for _, ch := range channels {
		sub, err := s.nc.Subscribe(ch.subject, ch.handler)
		if err != nil {
			return fmt.Errorf("订阅 %s 失败: %w", ch.subject, err)
		}
		s.subs = append(s.subs, sub)
	}

	s.log.Info("订阅同步事件成功")
	return nil
}

type TenantMigrationEvent struct {
	TenantID    string `json:"tenant_id"`
	OldServerID string `json:"old_server_id"`
	NewServerID string `json:"new_server_id"`
	NewTenantID string `json:"new_tenant_id"`
}

func (s *GatewayServer) subscribeMigrationEvents() error {
	if s.nc == nil {
		s.log.Warn("NATS 连接不可用，跳过迁移事件订阅")
		return nil
	}

	sub, err := s.nc.Subscribe("aibot.tenant.migrated", func(msg *nats.Msg) {
		var event TenantMigrationEvent
		if err := json.Unmarshal(msg.Data, &event); err != nil {
			s.log.Error("解析迁移事件失败", zap.Error(err))
			return
		}

		s.log.Info("收到租户迁移事件",
			zap.String("tenant_id", event.TenantID),
			zap.String("old_server_id", event.OldServerID),
			zap.String("new_server_id", event.NewServerID),
			zap.String("new_tenant_id", event.NewTenantID))
	})
	if err != nil {
		return fmt.Errorf("订阅迁移事件失败: %w", err)
	}
	s.subs = append(s.subs, sub)

	sub2, err := s.nc.Subscribe("aibot.tenant.config_changed", func(msg *nats.Msg) {
		var event map[string]interface{}
		if err := json.Unmarshal(msg.Data, &event); err != nil {
			s.log.Error("解析配置变更事件失败", zap.Error(err))
			return
		}

		s.log.Info("收到租户配置变更事件", zap.Any("event", event))
	})
	if err != nil {
		return fmt.Errorf("订阅配置变更事件失败: %w", err)
	}
	s.subs = append(s.subs, sub2)

	s.log.Info("订阅迁移和配置变更事件成功")
	return nil
}

type ChannelRequest struct {
	Channel   *ChannelInfo `json:"channel,omitempty"`
	ChannelID string       `json:"channel_id,omitempty"`
}

type ChannelResponse struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

func (s *GatewayServer) onChannelCreate(msg *nats.Msg) {
	var request ChannelRequest
	if err := json.Unmarshal(msg.Data, &request); err != nil {
		s.respondError(msg, "400", "Invalid JSON")
		return
	}

	if request.Channel == nil {
		s.respondError(msg, "400", "Missing channel info")
		return
	}

	channel := request.Channel

	if channel.ChannelID == "" {
		channel.ChannelID = uuid.New().String()
	}
	channel.Status = ChannelStatusActive

	if err := s.manager.CreateChannel(channel); err != nil {
		s.log.Error("创建渠道失败", zap.String("channel_id", channel.ChannelID), zap.Error(err))
		s.respondError(msg, "500", err.Error())
		return
	}

	s.log.Info("创建渠道",
		zap.String("channel_id", channel.ChannelID),
		zap.String("tenant_id", channel.TenantID),
		zap.String("channel_type", channel.ChannelType))

	data := map[string]interface{}{
		"channel_id": channel.ChannelID,
		"status":     channel.Status,
	}

	if s.httpHandler != nil && s.httpHandler.Config != nil {
		externalURL := s.httpHandler.Config.ExternalURL
		if externalURL != "" {
			data["webhook_url"] = s.buildWebhookURL(channel.ChannelType, channel.TenantSlug)
		}
	}

	s.respondOK(msg, data)
}

func (s *GatewayServer) onChannelUpdate(msg *nats.Msg) {
	var request ChannelRequest
	if err := json.Unmarshal(msg.Data, &request); err != nil {
		s.respondError(msg, "400", "Invalid JSON")
		return
	}

	if request.Channel == nil || request.Channel.ChannelID == "" {
		s.respondError(msg, "400", "Missing channel_id")
		return
	}

	if err := s.manager.UpdateChannel(request.Channel); err != nil {
		s.log.Error("更新渠道失败", zap.String("channel_id", request.Channel.ChannelID), zap.Error(err))
		s.respondError(msg, "500", err.Error())
		return
	}

	s.log.Info("更新渠道", zap.String("channel_id", request.Channel.ChannelID))
	s.respondOK(msg, nil)
}

func (s *GatewayServer) onChannelDestroy(msg *nats.Msg) {
	var request ChannelRequest
	if err := json.Unmarshal(msg.Data, &request); err != nil {
		s.respondError(msg, "400", "Invalid JSON")
		return
	}

	channelID := request.ChannelID
	if channelID == "" && request.Channel != nil {
		channelID = request.Channel.ChannelID
	}

	if channelID == "" {
		s.respondError(msg, "400", "Missing channel_id")
		return
	}

	ch, ok := s.manager.GetChannel(channelID)
	if !ok {
		s.respondError(msg, "404", "Channel not found")
		return
	}

	if err := s.manager.DeleteChannel(ch.ChannelType, channelID); err != nil {
		s.log.Error("删除渠道失败", zap.String("channel_id", channelID), zap.Error(err))
		s.respondError(msg, "500", err.Error())
		return
	}

	s.log.Info("删除渠道", zap.String("channel_id", channelID))
	s.respondOK(msg, nil)
}

func (s *GatewayServer) onChannelPause(msg *nats.Msg) {
	var request ChannelRequest
	if err := json.Unmarshal(msg.Data, &request); err != nil {
		s.respondError(msg, "400", "Invalid JSON")
		return
	}

	channelID := request.ChannelID
	if channelID == "" && request.Channel != nil {
		channelID = request.Channel.ChannelID
	}

	if channelID == "" {
		s.respondError(msg, "400", "Missing channel_id")
		return
	}

	ch, ok := s.manager.GetChannel(channelID)
	if !ok {
		s.respondError(msg, "404", "Channel not found")
		return
	}

	if err := s.manager.PauseChannel(ch.ChannelType, channelID); err != nil {
		s.log.Error("暂停渠道失败", zap.String("channel_id", channelID), zap.Error(err))
		s.respondError(msg, "500", err.Error())
		return
	}

	s.log.Info("暂停渠道", zap.String("channel_id", channelID))
	s.respondOK(msg, nil)
}

func (s *GatewayServer) onChannelResume(msg *nats.Msg) {
	var request ChannelRequest
	if err := json.Unmarshal(msg.Data, &request); err != nil {
		s.respondError(msg, "400", "Invalid JSON")
		return
	}

	channelID := request.ChannelID
	if channelID == "" && request.Channel != nil {
		channelID = request.Channel.ChannelID
	}

	if channelID == "" {
		s.respondError(msg, "400", "Missing channel_id")
		return
	}

	ch, ok := s.manager.GetChannel(channelID)
	if !ok {
		s.respondError(msg, "404", "Channel not found")
		return
	}

	if err := s.manager.ResumeChannel(ch.ChannelType, channelID); err != nil {
		s.log.Error("恢复渠道失败", zap.String("channel_id", channelID), zap.Error(err))
		s.respondError(msg, "500", err.Error())
		return
	}

	s.log.Info("恢复渠道", zap.String("channel_id", channelID))
	s.respondOK(msg, nil)
}

func (s *GatewayServer) onSyncCreate(msg *nats.Msg) {
	var request ChannelRequest
	if err := json.Unmarshal(msg.Data, &request); err != nil {
		s.log.Error("解析同步创建事件失败", zap.Error(err))
		return
	}

	if request.Channel == nil {
		s.log.Warn("同步创建事件缺少渠道信息")
		return
	}

	channel := request.Channel
	if channel.ChannelID == "" {
		channel.ChannelID = uuid.New().String()
	}
	channel.Status = ChannelStatusActive

	if err := s.manager.CreateChannel(channel); err != nil {
		s.log.Error("同步创建渠道失败", zap.String("channel_id", channel.ChannelID), zap.Error(err))
		return
	}

	s.log.Info("同步创建渠道",
		zap.String("channel_id", channel.ChannelID),
		zap.String("tenant_id", channel.TenantID),
		zap.String("channel_type", channel.ChannelType))
}

func (s *GatewayServer) onSyncUpdate(msg *nats.Msg) {
	var request ChannelRequest
	if err := json.Unmarshal(msg.Data, &request); err != nil {
		s.log.Error("解析同步更新事件失败", zap.Error(err))
		return
	}

	if request.Channel == nil || request.Channel.ChannelID == "" {
		s.log.Warn("同步更新事件缺少渠道信息")
		return
	}

	if err := s.manager.UpdateChannel(request.Channel); err != nil {
		s.log.Error("同步更新渠道失败", zap.String("channel_id", request.Channel.ChannelID), zap.Error(err))
		return
	}

	s.log.Info("同步更新渠道", zap.String("channel_id", request.Channel.ChannelID))
}

func (s *GatewayServer) onSyncDestroy(msg *nats.Msg) {
	var request ChannelRequest
	if err := json.Unmarshal(msg.Data, &request); err != nil {
		s.log.Error("解析同步删除事件失败", zap.Error(err))
		return
	}

	channelID := request.ChannelID
	if channelID == "" && request.Channel != nil {
		channelID = request.Channel.ChannelID
	}

	if channelID == "" {
		s.log.Warn("同步删除事件缺少 channel_id")
		return
	}

	ch, ok := s.manager.GetChannel(channelID)
	if !ok {
		s.log.Warn("同步删除渠道未找到", zap.String("channel_id", channelID))
		return
	}

	if err := s.manager.DeleteChannel(ch.ChannelType, channelID); err != nil {
		s.log.Error("同步删除渠道失败", zap.String("channel_id", channelID), zap.Error(err))
		return
	}

	s.log.Info("同步删除渠道", zap.String("channel_id", channelID))
}

func (s *GatewayServer) onSyncPause(msg *nats.Msg) {
	var request ChannelRequest
	if err := json.Unmarshal(msg.Data, &request); err != nil {
		s.log.Error("解析同步暂停事件失败", zap.Error(err))
		return
	}

	channelID := request.ChannelID
	if channelID == "" && request.Channel != nil {
		channelID = request.Channel.ChannelID
	}

	if channelID == "" {
		s.log.Warn("同步暂停事件缺少 channel_id")
		return
	}

	ch, ok := s.manager.GetChannel(channelID)
	if !ok {
		s.log.Warn("同步暂停渠道未找到", zap.String("channel_id", channelID))
		return
	}

	if err := s.manager.PauseChannel(ch.ChannelType, channelID); err != nil {
		s.log.Error("同步暂停渠道失败", zap.String("channel_id", channelID), zap.Error(err))
		return
	}

	s.log.Info("同步暂停渠道", zap.String("channel_id", channelID))
}

func (s *GatewayServer) onSyncResume(msg *nats.Msg) {
	var request ChannelRequest
	if err := json.Unmarshal(msg.Data, &request); err != nil {
		s.log.Error("解析同步恢复事件失败", zap.Error(err))
		return
	}

	channelID := request.ChannelID
	if channelID == "" && request.Channel != nil {
		channelID = request.Channel.ChannelID
	}

	if channelID == "" {
		s.log.Warn("同步恢复事件缺少 channel_id")
		return
	}

	ch, ok := s.manager.GetChannel(channelID)
	if !ok {
		s.log.Warn("同步恢复渠道未找到", zap.String("channel_id", channelID))
		return
	}

	if err := s.manager.ResumeChannel(ch.ChannelType, channelID); err != nil {
		s.log.Error("同步恢复渠道失败", zap.String("channel_id", channelID), zap.Error(err))
		return
	}

	s.log.Info("同步恢复渠道", zap.String("channel_id", channelID))
}

func (s *GatewayServer) buildWebhookURL(channelType, tenantSlug string) string {
	var typePrefix string
	switch channelType {
	case "weixin":
		typePrefix = "wx"
	case "feishu":
		typePrefix = "feishu"
	case "qqbot":
		typePrefix = "qqbot"
	default:
		typePrefix = channelType
	}

	externalURL := s.httpHandler.Config.ExternalURL
	prefix := s.httpHandler.Config.Prefix

	return fmt.Sprintf("%s%s/%s/%s", externalURL, prefix, typePrefix, tenantSlug)
}

func (s *GatewayServer) respondOK(msg *nats.Msg, data interface{}) {
	resp := ChannelResponse{
		Code:    0,
		Message: "success",
		Data:    data,
	}
	b, _ := json.Marshal(resp)
	msg.Respond(b)
}

func (s *GatewayServer) respondError(msg *nats.Msg, code, message string) {
	resp := ChannelResponse{
		Code:    1,
		Message: message,
	}
	b, _ := json.Marshal(resp)
	msg.Respond(b)
}

func (s *GatewayServer) GetChannel(channelID string) (*ChannelInfo, bool) {
	return s.manager.GetChannel(channelID)
}

func (s *GatewayServer) IsChannelActive(channelID string) bool {
	return s.manager.IsChannelActive(channelID)
}

func (s *GatewayServer) ListChannelsByTenant(tenantID string) []*ChannelInfo {
	return s.manager.ListChannelsByTenant(tenantID)
}

func (s *GatewayServer) ListAllChannels() []*ChannelInfo {
	return s.manager.ListAllChannels()
}

func (s *GatewayServer) InitChannelsFromAdmin(ctx context.Context, client *AIBotClient) error {
	channels, err := client.ListChannels(ctx, "", "")
	if err != nil {
		s.log.Error("从 Admin 获取渠道列表失败", zap.Error(err))
		return err
	}

	if err := s.manager.InitChannels(channels); err != nil {
		s.log.Error("初始化渠道失败", zap.Error(err))
		return err
	}

	s.log.Info("渠道初始化完成", zap.Int("count", len(channels)))
	return nil
}
