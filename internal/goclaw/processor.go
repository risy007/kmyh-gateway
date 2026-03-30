package goclaw

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

type AgentRequest struct {
	CustomerID    string
	Message       interface{}
	Channel       string
	ChannelUserID string
	Extra         map[string]interface{}
}

type AgentResponse struct {
	Content    string
	Type       string
	Recipients []ResponseRecipient
}

type ResponseRecipient struct {
	ChannelType string
	UserID      string
}

type AgentProcessor interface {
	Process(ctx context.Context, req *AgentRequest) (*AgentResponse, error)
}

type GoclawAgentProcessor struct {
	log           *zap.SugaredLogger
	botMap        map[string]GoclawBotConfig
	wsClientCache map[string]*simpleWSClient
	wsClientMu    sync.RWMutex
	mu            sync.Mutex
}

type simpleWSClient struct {
	conn   *websocket.Conn
	url    string
	apiKey string
	log    *zap.SugaredLogger
}

func (c *simpleWSClient) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

func NewGoclawAgentProcessor(logger *zap.Logger) *GoclawAgentProcessor {
	return &GoclawAgentProcessor{
		log:           logger.With(zap.Namespace("[GoclawAgentProcessor]")).Sugar(),
		botMap:        make(map[string]GoclawBotConfig),
		wsClientCache: make(map[string]*simpleWSClient),
	}
}

func (p *GoclawAgentProcessor) UpdateBotConfig(bots []GoclawBotConfig) {
	p.mu.Lock()
	defer p.mu.Unlock()

	newBotMap := make(map[string]GoclawBotConfig)
	for _, bot := range bots {
		newBotMap[bot.UserID] = bot
	}
	p.botMap = newBotMap

	p.wsClientMu.Lock()
	for _, client := range p.wsClientCache {
		client.Close()
	}
	p.wsClientCache = make(map[string]*simpleWSClient)
	p.wsClientMu.Unlock()
}

func (p *GoclawAgentProcessor) Process(ctx context.Context, req *AgentRequest) (*AgentResponse, error) {
	agentConfigRaw, ok := req.Extra["agent_config"].(json.RawMessage)
	if !ok {
		return nil, fmt.Errorf("agent_config not found")
	}

	var config GoclawBotConfig
	if err := json.Unmarshal(agentConfigRaw, &config); err != nil {
		return nil, fmt.Errorf("解析 agent config 失败: %w", err)
	}

	messageContent := p.extractMessageContent(req.Message)

	client, err := p.connectWebSocket(ctx, req.CustomerID, &config)
	if err != nil {
		return nil, fmt.Errorf("连接失败: %w", err)
	}
	defer client.Close()

	chatParams := ChatSendParams{
		Message: messageContent,
		AgentID: config.AgentID,
	}

	content, err := p.sendChatMessage(ctx, client.conn, &chatParams)
	if err != nil {
		return nil, fmt.Errorf("发送消息失败: %w", err)
	}

	return &AgentResponse{
		Content: content,
		Type:    "markdown",
		Recipients: []ResponseRecipient{
			{
				ChannelType: req.Channel,
				UserID:      req.ChannelUserID,
			},
		},
	}, nil
}

func (p *GoclawAgentProcessor) connectWebSocket(ctx context.Context, tenantID string, config *GoclawBotConfig) (*simpleWSClient, error) {
	wsURL := config.BaseURL
	if !strings.HasPrefix(wsURL, "ws") && !strings.HasPrefix(wsURL, "http") {
		wsURL = "ws://" + wsURL
	}
	if strings.HasPrefix(wsURL, "http://") {
		wsURL = "ws://" + wsURL[7:]
	} else if strings.HasPrefix(wsURL, "https://") {
		wsURL = "wss://" + wsURL[8:]
	}
	if !strings.HasSuffix(wsURL, "/ws") && !strings.HasSuffix(wsURL, "/ws/") {
		wsURL = strings.TrimSuffix(wsURL, "/") + "/ws"
	}

	dialer := websocket.Dialer{
		ReadBufferSize:  512,
		WriteBufferSize: 512,
	}

	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("WebSocket 连接失败: %w", err)
	}

	conn.SetReadDeadline(time.Now().Add(wsReadDeadline))
	conn.SetWriteDeadline(time.Now().Add(wsWriteDeadline))

	connectParams := map[string]string{
		"token":     config.APIKey,
		"tenant_id": tenantID,
	}
	paramsJSON, _ := json.Marshal(connectParams)
	connectFrame := wsFrame{
		Type:   "req",
		ID:     "connect-1",
		Method: "connect",
		Params: paramsJSON,
	}
	if err := conn.WriteJSON(connectFrame); err != nil {
		conn.Close()
		return nil, fmt.Errorf("发送 connect 请求失败: %w", err)
	}

	respFrame := wsFrame{}
	if err := conn.ReadJSON(&respFrame); err != nil {
		conn.Close()
		return nil, fmt.Errorf("读取 connect 响应失败: %w", err)
	}

	if !respFrame.OK {
		conn.Close()
		errMsg := "unknown error"
		if respFrame.Error != nil {
			errMsg = respFrame.Error.Message
		}
		return nil, fmt.Errorf("GoClaw 连接认证失败: %s", errMsg)
	}

	return &simpleWSClient{
		conn:   conn,
		url:    wsURL,
		apiKey: config.APIKey,
		log:    p.log,
	}, nil
}

func (p *GoclawAgentProcessor) sendChatMessage(ctx context.Context, conn *websocket.Conn, params *ChatSendParams) (string, error) {
	paramsJSON, _ := json.Marshal(params)
	frameID := fmt.Sprintf("chat-%d", time.Now().UnixNano())
	frame := wsFrame{
		Type:   "req",
		ID:     frameID,
		Method: "chat.send",
		Params: paramsJSON,
	}

	if err := conn.WriteJSON(frame); err != nil {
		return "", fmt.Errorf("发送请求失败: %w", err)
	}

	var fullContent string

	for {
		respFrame := wsFrame{}
		conn.SetReadDeadline(time.Now().Add(wsReadDeadline))
		if err := conn.ReadJSON(&respFrame); err != nil {
			return fullContent, fmt.Errorf("读取响应失败: %w", err)
		}

		if respFrame.Type == "event" {
			switch respFrame.Event {
			case "chunk":
				var chunk ChunkEvent
				if err := json.Unmarshal(respFrame.Payload, &chunk); err == nil && chunk.Content != "" {
					fullContent += chunk.Content
				}
			case "run.completed":
				return fullContent, nil
			}
			continue
		}

		if respFrame.ID != frameID {
			continue
		}

		if !respFrame.OK {
			errMsg := "unknown error"
			if respFrame.Error != nil {
				errMsg = respFrame.Error.Message
			}
			return fullContent, fmt.Errorf("chat.send 错误: %s", errMsg)
		}

		var chatResp ChatSendResponse
		if err := json.Unmarshal(respFrame.Payload, &chatResp); err != nil {
			return fullContent, fmt.Errorf("解析响应失败: %w", err)
		}

		return chatResp.Content, nil
	}
}

func (p *GoclawAgentProcessor) extractMessageContent(msg interface{}) string {
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

type wsFrame struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	OK      bool            `json:"ok,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Error   *wsError        `json:"error,omitempty"`
	Event   string          `json:"event,omitempty"`
}

type wsError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
