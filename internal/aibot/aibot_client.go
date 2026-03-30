package aibot

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	kmyhconfig "github.com/risy007/kmyh-config"
)

type Options struct {
	Timeout    time.Duration
	RetryCount int
}

type Option func(*Options)

func defaultOptions() Options {
	return Options{
		Timeout:    5 * time.Second,
		RetryCount: 3,
	}
}

func WithTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		o.Timeout = timeout
	}
}

func WithRetryCount(count int) Option {
	return func(o *Options) {
		o.RetryCount = count
	}
}

type TenantBindInfo struct {
	TenantID      string          `json:"tenant_id"`
	TenantSlug    string          `json:"tenant_slug"`
	UnifiedUserID string          `json:"unified_user_id"`
	AgentID       string          `json:"agent_id"`
	Channel       string          `json:"channel"`
	ChannelUserID string          `json:"channel_user_id"`
	AgentType     string          `json:"agent_type"`
	AgentConfig   json.RawMessage `json:"agent_config,omitempty"`
	Extra         json.RawMessage `json:"extra,omitempty"`
	Found         bool            `json:"found"`
}

type AIBotClient struct {
	nc   *nats.Conn
	opts Options
}

func NewAIBotClient(nc *nats.Conn, opts ...Option) *AIBotClient {
	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}
	return &AIBotClient{nc: nc, opts: o}
}

func (c *AIBotClient) GetBindInfo(ctx context.Context, channel, channelUserID string) (*TenantBindInfo, error) {
	resp, err := c.GetCustomerBindInfo(ctx, &kmyhconfig.GetCustomerBindInfoRequest{
		Channel:       channel,
		ChannelUserID: channelUserID,
	})
	if err != nil {
		return nil, err
	}

	if !resp.Found || resp.Info == nil {
		return nil, fmt.Errorf("未找到租户绑定信息: %s", resp.Error)
	}

	return &TenantBindInfo{
		TenantID:      resp.Info.TenantID,
		UnifiedUserID: resp.Info.UnifiedUserID,
		AgentID:       resp.Info.AgentID,
		Channel:       channel,
		ChannelUserID: channelUserID,
		Found:         true,
	}, nil
}

func (c *AIBotClient) GetCustomerBindInfo(ctx context.Context, req *kmyhconfig.GetCustomerBindInfoRequest) (*kmyhconfig.GetCustomerBindInfoResponse, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()

	resp, err := c.nc.RequestWithContext(ctx, "aibot.bind_info", data)
	if err != nil {
		return nil, fmt.Errorf("请求 aibot.bind_info 失败: %w", err)
	}

	var result struct {
		Data kmyhconfig.GetCustomerBindInfoResponse `json:"data"`
	}
	if err := json.Unmarshal(resp.Data, &result); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}

	return &result.Data, nil
}

func (c *AIBotClient) ListChannels(ctx context.Context, channelType, status string) ([]*ChannelInfo, error) {
	req := struct {
		ChannelType string `json:"channel_type,omitempty"`
		Status      string `json:"status,omitempty"`
	}{
		ChannelType: channelType,
		Status:      status,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()

	resp, err := c.nc.RequestWithContext(ctx, "aibot.channels.list", data)
	if err != nil {
		return nil, fmt.Errorf("请求 aibot.channels.list 失败: %w", err)
	}

	var result struct {
		Data struct {
			Channels []*ChannelInfo `json:"channels"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Data, &result); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}

	return result.Data.Channels, nil
}

type TenantInfo struct {
	TenantID string `json:"tenant_id"`
	AgentID  string `json:"agent_id"`
}

type TenantLookupFunc func(channelType, channelUserID string) (*TenantInfo, error)

type TenantLookupError struct {
	Message string
}

func (e *TenantLookupError) Error() string {
	return e.Message
}

var (
	DefaultTenantLookup TenantLookupFunc
	tenantLookupMu      sync.RWMutex
)

func SetTenantLookup(fn TenantLookupFunc) {
	tenantLookupMu.Lock()
	DefaultTenantLookup = fn
	tenantLookupMu.Unlock()
}

func LookupTenant(channelType, channelUserID string) (*TenantInfo, error) {
	tenantLookupMu.RLock()
	fn := DefaultTenantLookup
	tenantLookupMu.RUnlock()

	if fn == nil {
		return nil, &TenantLookupError{Message: "租户查询函数未配置，请先调用 SetTenantLookup 设置"}
	}
	return fn(channelType, channelUserID)
}

func NewTenantLookupFunc(client *AIBotClient) TenantLookupFunc {
	return func(channelType, channelUserID string) (*TenantInfo, error) {
		ctx := context.Background()
		info, err := client.GetBindInfo(ctx, channelType, channelUserID)
		if err != nil {
			return nil, fmt.Errorf("查询租户信息失败: %w", err)
		}
		return &TenantInfo{
			TenantID: info.TenantID,
			AgentID:  info.AgentID,
		}, nil
	}
}
