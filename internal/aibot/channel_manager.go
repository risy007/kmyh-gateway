package aibot

import (
	"encoding/json"
	"fmt"
	"sync"
)

type ChannelStatus string

const (
	ChannelStatusActive   ChannelStatus = "active"
	ChannelStatusPaused   ChannelStatus = "paused"
	ChannelStatusDisabled ChannelStatus = "disabled"
)

type ChannelInfo struct {
	ChannelID   string          `json:"channel_id"`
	TenantID    string          `json:"tenant_id"`
	TenantSlug  string          `json:"tenant_slug"`
	ChannelType string          `json:"channel_type"`
	Status      ChannelStatus   `json:"status"`
	Config      json.RawMessage `json:"config,omitempty"`
	CreatedAt   string          `json:"created_at,omitempty"`
	UpdatedAt   string          `json:"updated_at,omitempty"`
}

type ChannelHandler interface {
	GetChannelType() string
	InitChannel(channel *ChannelInfo) error
	UpdateChannel(channel *ChannelInfo) error
	DeleteChannel(channelID string) error
	PauseChannel(channelID string) error
}

type ChannelManager struct {
	mu       sync.RWMutex
	handlers map[string]ChannelHandler
	channels map[string]*ChannelInfo
}

func NewChannelManager() *ChannelManager {
	return &ChannelManager{
		handlers: make(map[string]ChannelHandler),
		channels: make(map[string]*ChannelInfo),
	}
}

func (m *ChannelManager) Register(handler ChannelHandler) {
	m.mu.Lock()
	defer m.mu.Unlock()

	channelType := handler.GetChannelType()
	m.handlers[channelType] = handler
}

func (m *ChannelManager) GetHandler(channelType string) (ChannelHandler, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	handler, ok := m.handlers[channelType]
	return handler, ok
}

func (m *ChannelManager) InitChannels(channels []*ChannelInfo) error {
	var errs []error

	for _, ch := range channels {
		handler, ok := m.GetHandler(ch.ChannelType)
		if !ok {
			continue
		}

		if err := handler.InitChannel(ch); err != nil {
			errs = append(errs, fmt.Errorf("初始化渠道 %s (%s) 失败: %w", ch.ChannelID, ch.ChannelType, err))
			continue
		}

		m.channels[ch.TenantSlug] = ch
	}

	if len(errs) > 0 {
		return fmt.Errorf("渠道初始化部分失败: %v", errs)
	}
	return nil
}

func (m *ChannelManager) CreateChannel(channel *ChannelInfo) error {
	handler, ok := m.GetHandler(channel.ChannelType)
	if !ok {
		return fmt.Errorf("未找到渠道类型 %s 的处理器", channel.ChannelType)
	}

	ch := &ChannelInfo{
		ChannelID:   channel.ChannelID,
		TenantID:    channel.TenantID,
		TenantSlug:  channel.TenantSlug,
		ChannelType: channel.ChannelType,
		Status:      ChannelStatusActive,
		Config:      channel.Config,
	}

	if err := handler.InitChannel(ch); err != nil {
		return err
	}

	m.channels[ch.TenantSlug] = ch
	return nil
}

func (m *ChannelManager) UpdateChannel(channel *ChannelInfo) error {
	handler, ok := m.GetHandler(channel.ChannelType)
	if !ok {
		return fmt.Errorf("未找到渠道类型 %s 的处理器", channel.ChannelType)
	}
	return handler.UpdateChannel(channel)
}

func (m *ChannelManager) DeleteChannel(channelType, channelID string) error {
	handler, ok := m.GetHandler(channelType)
	if !ok {
		return fmt.Errorf("未找到渠道类型 %s 的处理器", channelType)
	}

	if err := handler.DeleteChannel(channelID); err != nil {
		return err
	}

	for slug, ch := range m.channels {
		if ch.ChannelID == channelID || slug == channelID {
			delete(m.channels, slug)
			break
		}
	}
	return nil
}

func (m *ChannelManager) IsChannelActive(channelID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	ch, ok := m.channels[channelID]
	if !ok {
		return false
	}
	return ch.Status == ChannelStatusActive
}

func (m *ChannelManager) GetChannelBySlug(slug string) *ChannelInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.channels[slug]
}

func (m *ChannelManager) PauseChannel(channelType, channelID string) error {
	handler, ok := m.GetHandler(channelType)
	if !ok {
		return fmt.Errorf("未找到渠道类型 %s 的处理器", channelType)
	}

	if err := handler.PauseChannel(channelID); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for slug, ch := range m.channels {
		if ch.ChannelID == channelID || slug == channelID {
			ch.Status = ChannelStatusPaused
			m.channels[slug] = ch
			break
		}
	}
	return nil
}

func (m *ChannelManager) ResumeChannel(channelType, channelID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for slug, ch := range m.channels {
		if ch.ChannelID == channelID || slug == channelID {
			ch.Status = ChannelStatusActive
			m.channels[slug] = ch
			return nil
		}
	}
	return fmt.Errorf("未找到渠道 %s", channelID)
}

func (m *ChannelManager) GetRegisteredTypes() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	types := make([]string, 0, len(m.handlers))
	for t := range m.handlers {
		types = append(types, t)
	}
	return types
}
