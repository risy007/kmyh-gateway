package agent

import (
	"context"
	"encoding/json"
	"fmt"

	kmyhconfig "github.com/risy007/kmyh-config"
	"github.com/risy007/kmyh-gateway/internal/goclaw"
	"go.uber.org/zap"
)

// GoclawAgent 使用 internal/goclaw.Dial 进行一次性 WS 连接和聊天
// 所有 WS 连接/认证/帧解析逻辑统一在 internal/goclaw/dial.go
type GoclawAgent struct {
	log *zap.SugaredLogger
}

func NewGoclawAgent(logger *zap.Logger) (*GoclawAgent, error) {
	log := logger.With(zap.Namespace("[GoclawAgent]")).Sugar()
	return &GoclawAgent{
		log: log,
	}, nil
}

func (a *GoclawAgent) Type() string {
	return "goclaw"
}

func (a *GoclawAgent) Process(ctx context.Context, req *AgentRequest) (*AgentResponse, error) {
	agentConfigRaw, ok := req.Extra["agent_config"].(json.RawMessage)
	if !ok {
		return nil, fmt.Errorf("agent_config not found")
	}

	var config kmyhconfig.GoclawBotConfig
	a.log.Infow("AgentConfig raw JSON", "json", string(agentConfigRaw))
	if err := json.Unmarshal(agentConfigRaw, &config); err != nil {
		return nil, fmt.Errorf("解析 agent config 失败: %w", err)
	}

	messageContent := goclaw.ExtractMessageContent(req.Message)

	unifiedUserID := ""
	sessionKey := ""
	if bindInfo, ok := req.Extra["bind_info"]; ok {
		switch v := bindInfo.(type) {
		case *kmyhconfig.CustomerBindInfo:
			if v.UnifiedUserID != "" {
				unifiedUserID = v.UnifiedUserID
				sessionKey = v.TenantSlug + ":" + v.UnifiedUserID
			}
		case kmyhconfig.CustomerBindInfo:
			if v.UnifiedUserID != "" {
				unifiedUserID = v.UnifiedUserID
				sessionKey = v.TenantSlug + ":" + v.UnifiedUserID
			}
		}
	}

	conn, err := goclaw.Dial(ctx, goclaw.DialConfig{
		BaseURL:  config.BaseURL,
		APIKey:   config.APIKey,
		UserID:   unifiedUserID,
		TenantID: req.CustomerID,
	})
	if err != nil {
		return nil, fmt.Errorf("连接失败: %w", err)
	}
	defer conn.Close()

	content, err := conn.ChatSendSync(ctx, &goclaw.ChatSendParams{
		Message:    messageContent,
		AgentID:    config.AgentID,
		SessionKey: sessionKey,
	})
	if err != nil {
		return nil, fmt.Errorf("发送消息失败: %w", err)
	}

	return &AgentResponse{
		Content: content,
		Type:    "markdown",
		Recipients: []Recipient{
			{
				ChannelType: req.Channel,
				UserID:      req.ChannelUserID,
			},
		},
	}, nil
}
