package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	kmyhconfig "github.com/risy007/kmyh-config"
	"go.uber.org/zap"
)

const (
	wsPingInterval  = 30 * time.Second
	wsReadDeadline  = 60 * time.Second
	wsWriteDeadline = 10 * time.Second
)

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
	if err := json.Unmarshal(agentConfigRaw, &config); err != nil {
		return nil, fmt.Errorf("解析 agent config 失败: %w", err)
	}

	messageContent := a.extractMessageContent(req.Message)

	conn, err := a.connectWebSocket(ctx, req.CustomerID, &config)
	if err != nil {
		return nil, fmt.Errorf("连接失败: %w", err)
	}
	defer conn.Close()

	chatParams := ChatSendParams{
		Message: messageContent,
		AgentID: config.AgentID,
	}

	content, err := a.sendChatMessage(ctx, conn, &chatParams)
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

func (a *GoclawAgent) connectWebSocket(ctx context.Context, tenantID string, config *kmyhconfig.GoclawBotConfig) (*websocket.Conn, error) {
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
	connectFrame := WSFrame{
		Type:   "req",
		ID:     "connect-1",
		Method: "connect",
		Params: paramsJSON,
	}
	if err := conn.WriteJSON(connectFrame); err != nil {
		conn.Close()
		return nil, fmt.Errorf("发送 connect 请求失败: %w", err)
	}

	respFrame := WSFrame{}
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

	return conn, nil
}

func (a *GoclawAgent) sendChatMessage(ctx context.Context, conn *websocket.Conn, params *ChatSendParams) (string, error) {
	paramsJSON, _ := json.Marshal(params)
	frameID := fmt.Sprintf("chat-%d", time.Now().UnixNano())
	frame := WSFrame{
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
		respFrame := WSFrame{}
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

func (a *GoclawAgent) extractMessageContent(msg interface{}) string {
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

type WSFrame struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	OK      bool            `json:"ok,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Error   *WSError        `json:"error,omitempty"`
	Event   string          `json:"event,omitempty"`
}

type WSError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ChatSendParams struct {
	Message    string `json:"message"`
	AgentID    string `json:"agentId,omitempty"`
	SessionKey string `json:"sessionKey,omitempty"`
}

type ChatSendResponse struct {
	Content string `json:"content,omitempty"`
}

type ChunkEvent struct {
	Content string `json:"content"`
}
