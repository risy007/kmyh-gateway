package aibot

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

type GatewayServer struct {
	mu       sync.RWMutex
	log      *zap.Logger
	ctx      context.Context
	nc       *nats.Conn
	micro    micro.Service
	channels map[string]*ChannelInfo
	manager  *ChannelManager
}

type serverInParams struct {
	fx.In
	Logger  *zap.Logger
	Ctx     context.Context
	NC      *nats.Conn
	Manager *ChannelManager
}

func NewGatewayServer(in serverInParams) (*GatewayServer, error) {
	log := in.Logger.With(zap.Namespace("[GatewayServer]"))

	server := &GatewayServer{
		log:      log,
		ctx:      in.Ctx,
		nc:       in.NC,
		manager:  in.Manager,
		channels: make(map[string]*ChannelInfo),
	}

	log.Info("GatewayServer 初始化完成")
	return server, nil
}

func (s *GatewayServer) Start() error {
	s.log.Info("启动 GatewayServer")

	if s.nc == nil {
		return fmt.Errorf("NATS 连接未初始化")
	}

	if err := s.registerMicroService(); err != nil {
		s.log.Error("注册NATS micro服务失败", zap.Error(err))
		return err
	}

	return nil
}

func (s *GatewayServer) Stop() error {
	s.log.Info("停止 GatewayServer")
	if s.micro != nil {
		return s.micro.Stop()
	}
	return nil
}

func (s *GatewayServer) registerMicroService() error {
	s.log.Info("注册 Gateway NATS micro 服务")

	srv, err := micro.AddService(s.nc, micro.Config{
		Name:        "gateway",
		Version:     "1.0.0",
		Description: "网关渠道管理服务",
	})
	if err != nil {
		return err
	}

	s.micro = srv
	grp := srv.AddGroup("gateway.channels")

	grp.AddEndpoint("create", micro.HandlerFunc(s.handleChannelCreate))
	grp.AddEndpoint("update", micro.HandlerFunc(s.handleChannelUpdate))
	grp.AddEndpoint("destroy", micro.HandlerFunc(s.handleChannelDestroy))
	grp.AddEndpoint("pause", micro.HandlerFunc(s.handleChannelPause))

	s.log.Info("Gateway NATS micro 服务注册成功")
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

func (s *GatewayServer) handleChannelCreate(req micro.Request) {
	var request ChannelRequest
	if err := json.Unmarshal(req.Data(), &request); err != nil {
		req.Error("400", "Invalid JSON", nil)
		return
	}

	if request.Channel == nil {
		req.Error("400", "Missing channel info", nil)
		return
	}

	channel := request.Channel

	if channel.ChannelID == "" {
		channel.ChannelID = uuid.New().String()
	}
	channel.Status = ChannelStatusActive

	if err := s.manager.CreateChannel(channel); err != nil {
		s.log.Error("创建渠道失败", zap.String("channel_id", channel.ChannelID), zap.Error(err))
		req.Error("500", err.Error(), nil)
		return
	}

	s.mu.Lock()
	s.channels[channel.ChannelID] = channel
	s.mu.Unlock()

	s.log.Info("创建渠道",
		zap.String("channel_id", channel.ChannelID),
		zap.String("tenant_id", channel.TenantID),
		zap.String("channel_type", channel.ChannelType))

	req.RespondJSON(ChannelResponse{
		Code:    0,
		Message: "success",
		Data: map[string]interface{}{
			"channel_id": channel.ChannelID,
			"status":     channel.Status,
		},
	})
}

func (s *GatewayServer) handleChannelUpdate(req micro.Request) {
	var request ChannelRequest
	if err := json.Unmarshal(req.Data(), &request); err != nil {
		req.Error("400", "Invalid JSON", nil)
		return
	}

	if request.Channel == nil || request.Channel.ChannelID == "" {
		req.Error("400", "Missing channel_id", nil)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.channels[request.Channel.ChannelID]
	if !ok {
		req.Error("404", "Channel not found", nil)
		return
	}

	updated := existing
	if request.Channel.TenantID != "" {
		updated.TenantID = request.Channel.TenantID
	}
	if request.Channel.ChannelType != "" {
		updated.ChannelType = request.Channel.ChannelType
	}
	if request.Channel.Status != "" {
		updated.Status = request.Channel.Status
	}
	if request.Channel.Config != nil {
		updated.Config = request.Channel.Config
	}

	if err := s.manager.UpdateChannel(updated); err != nil {
		s.log.Error("更新渠道失败", zap.String("channel_id", updated.ChannelID), zap.Error(err))
		req.Error("500", err.Error(), nil)
		return
	}

	s.channels[updated.ChannelID] = updated

	s.log.Info("更新渠道", zap.String("channel_id", updated.ChannelID))

	req.RespondJSON(ChannelResponse{
		Code:    0,
		Message: "success",
	})
}

func (s *GatewayServer) handleChannelDestroy(req micro.Request) {
	var request ChannelRequest
	if err := json.Unmarshal(req.Data(), &request); err != nil {
		req.Error("400", "Invalid JSON", nil)
		return
	}

	channelID := request.ChannelID
	if channelID == "" && request.Channel != nil {
		channelID = request.Channel.ChannelID
	}

	if channelID == "" {
		req.Error("400", "Missing channel_id", nil)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	channel, ok := s.channels[channelID]
	if !ok {
		req.Error("404", "Channel not found", nil)
		return
	}

	if err := s.manager.DeleteChannel(channel.ChannelType, channelID); err != nil {
		s.log.Error("删除渠道失败", zap.String("channel_id", channelID), zap.Error(err))
		req.Error("500", err.Error(), nil)
		return
	}

	delete(s.channels, channelID)

	s.log.Info("删除渠道", zap.String("channel_id", channelID))

	req.RespondJSON(ChannelResponse{
		Code:    0,
		Message: "success",
	})
}

func (s *GatewayServer) handleChannelPause(req micro.Request) {
	var request ChannelRequest
	if err := json.Unmarshal(req.Data(), &request); err != nil {
		req.Error("400", "Invalid JSON", nil)
		return
	}

	channelID := request.ChannelID
	if channelID == "" && request.Channel != nil {
		channelID = request.Channel.ChannelID
	}

	if channelID == "" {
		req.Error("400", "Missing channel_id", nil)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	channel, ok := s.channels[channelID]
	if !ok {
		req.Error("404", "Channel not found", nil)
		return
	}

	if err := s.manager.PauseChannel(channel.ChannelType, channelID); err != nil {
		s.log.Error("暂停渠道失败", zap.String("channel_id", channelID), zap.Error(err))
		req.Error("500", err.Error(), nil)
		return
	}

	channel.Status = ChannelStatusPaused
	s.channels[channelID] = channel

	s.log.Info("暂停渠道", zap.String("channel_id", channelID))

	req.RespondJSON(ChannelResponse{
		Code:    0,
		Message: "success",
	})
}

func (s *GatewayServer) GetChannel(channelID string) (*ChannelInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	channel, ok := s.channels[channelID]
	return channel, ok
}

func (s *GatewayServer) IsChannelActive(channelID string) bool {
	channel, ok := s.GetChannel(channelID)
	return ok && channel.Status == ChannelStatusActive
}

func (s *GatewayServer) ListChannelsByTenant(tenantID string) []*ChannelInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var channels []*ChannelInfo
	for _, ch := range s.channels {
		if ch.TenantID == tenantID {
			channels = append(channels, ch)
		}
	}
	return channels
}

func (s *GatewayServer) ListAllChannels() []*ChannelInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	channels := make([]*ChannelInfo, 0, len(s.channels))
	for _, ch := range s.channels {
		channels = append(channels, ch)
	}
	return channels
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

	for _, ch := range channels {
		s.mu.Lock()
		s.channels[ch.ChannelID] = ch
		s.mu.Unlock()
	}

	s.log.Info("渠道初始化完成", zap.Int("count", len(channels)))
	return nil
}
