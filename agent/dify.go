package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	kmyhconfig "github.com/risy007/kmyh-config"
	"go.uber.org/zap"
)

type DifyAgent struct {
	log    *zap.SugaredLogger
	client *http.Client
}

func NewDifyAgent(logger *zap.Logger) (*DifyAgent, error) {
	log := logger.With(zap.Namespace("[DifyAgent]")).Sugar()
	return &DifyAgent{
		log:    log,
		client: &http.Client{Timeout: 120 * time.Second},
	}, nil
}

func (a *DifyAgent) Type() string {
	return "dify"
}

func (a *DifyAgent) Process(ctx context.Context, req *AgentRequest) (*AgentResponse, error) {
	agentConfigRaw, ok := req.Extra["agent_config"].(json.RawMessage)
	if !ok {
		return nil, fmt.Errorf("agent_config not found")
	}

	var config kmyhconfig.DifyBotConfig
	if err := json.Unmarshal(agentConfigRaw, &config); err != nil {
		return nil, fmt.Errorf("解析 agent config 失败: %w", err)
	}

	messageContent := a.extractMessageContent(req.Message)

	difyReq := DifyChatRequest{
		Query: messageContent,
		User:  req.CustomerID,
	}

	jsonData, err := json.Marshal(difyReq)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	fullURL := config.BaseURL
	if !strings.HasSuffix(fullURL, "/chat-messages") {
		fullURL = strings.TrimSuffix(fullURL, "/") + "/chat-messages"
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", fullURL, strings.NewReader(string(jsonData)))
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+config.APIKey)

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API 返回错误状态码 %d: %s", resp.StatusCode, string(respBody))
	}

	var difyResp DifyChatResponse
	if err := json.Unmarshal(respBody, &difyResp); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}

	return &AgentResponse{
		Content: difyResp.Answer,
		Type:    "markdown",
		Recipients: []Recipient{
			{
				ChannelType: req.Channel,
				UserID:      req.ChannelUserID,
			},
		},
	}, nil
}

func (a *DifyAgent) extractMessageContent(msg interface{}) string {
	switch v := msg.(type) {
	case string:
		return v
	case map[string]interface{}:
		if content, ok := v["content"].(string); ok {
			return content
		}
		data, _ := json.Marshal(v)
		return string(data)
	default:
		data, _ := json.Marshal(v)
		return string(data)
	}
}

type DifyChatRequest struct {
	Query string `json:"query"`
	User  string `json:"user"`
}

type DifyChatResponse struct {
	Answer string `json:"answer"`
}
