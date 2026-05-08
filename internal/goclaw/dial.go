package goclaw

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	wsReadDeadline  = 300 * time.Second
	wsWriteDeadline = 10 * time.Second
)

// DialConfig 一次性 WS 拨号配置
type DialConfig struct {
	BaseURL  string // GoClaw 服务器地址 (http/https/ws/wss)
	APIKey   string // 认证令牌
	UserID   string // 用户 ID，作为 connect 帧 user_id（GoClaw 必需）
	TenantID string // 租户 ID，作为 connect 帧 tenant_id（用于租户范围限定）
}

// DialConn 一次性 WS 连接（connect 已完成），调用方负责 Close
type DialConn struct {
	conn *websocket.Conn
}

// Close 关闭底层 WebSocket 连接
func (d *DialConn) Close() error {
	if d.conn != nil {
		return d.conn.Close()
	}
	return nil
}

// Dial 建立一次性 WS 连接并完成 GoClaw connect 握手
func Dial(ctx context.Context, cfg DialConfig) (*DialConn, error) {
	wsURL := buildWSURL(cfg.BaseURL)

	dialer := websocket.Dialer{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
	}

	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("WebSocket 连接失败: %w", err)
	}

	conn.SetReadDeadline(time.Now().Add(wsReadDeadline))
	conn.SetWriteDeadline(time.Now().Add(wsWriteDeadline))

	// 发送 connect 帧
	connectParams := map[string]string{
		"token":     cfg.APIKey,
		"user_id":   cfg.UserID,
		"tenant_id": cfg.TenantID,
	}
	connectFrame := WSRequestFrame{
		Type:   WSFrameRequest,
		ID:     "connect-1",
		Method: "connect",
		Params: connectParams,
	}
	if err := conn.WriteJSON(connectFrame); err != nil {
		conn.Close()
		return nil, fmt.Errorf("发送 connect 请求失败: %w", err)
	}

	// 读取 connect 响应
	var respFrame WSResponseFrame
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

	return &DialConn{conn: conn}, nil
}

// ChatSendSync 通过一次性连接发送 chat.send 并阻塞等待完整响应
// 自动处理 chunk 流式事件和 run.completed 事件
func (d *DialConn) ChatSendSync(ctx context.Context, params *ChatSendParams) (string, error) {
	frameID := fmt.Sprintf("chat-%d", time.Now().UnixNano())
	frame := WSRequestFrame{
		Type:   WSFrameRequest,
		ID:     frameID,
		Method: "chat.send",
		Params: params,
	}

	if err := d.conn.WriteJSON(frame); err != nil {
		return "", fmt.Errorf("发送请求失败: %w", err)
	}

	var fullContent string

	for {
		d.conn.SetReadDeadline(time.Now().Add(wsReadDeadline))

		_, data, err := d.conn.ReadMessage()
		if err != nil {
			if fullContent != "" {
				return fullContent, nil // 返回已收到的内容
			}
			return "", fmt.Errorf("读取响应失败: %w", err)
		}

		var raw struct {
			Type  string `json:"type"`
			Event string `json:"event,omitempty"`
			ID    string `json:"id,omitempty"`
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			continue
		}

		// 处理事件帧
		if raw.Type == "event" {
			switch raw.Event {
			case EventChunk:
				var evt struct {
					Payload ChunkEvent `json:"payload"`
				}
				if err := json.Unmarshal(data, &evt); err == nil && evt.Payload.Content != "" {
					fullContent += evt.Payload.Content
				}
			case EventRunCompleted:
				return fullContent, nil
			}
			continue
		}

		// 处理响应帧
		if raw.Type == "res" && raw.ID == frameID {
			var resp WSResponseFrame
			if err := json.Unmarshal(data, &resp); err != nil {
				return fullContent, fmt.Errorf("解析响应失败: %w", err)
			}

			if !resp.OK {
				errMsg := "unknown error"
				if resp.Error != nil {
					errMsg = resp.Error.Message
				}
				return fullContent, fmt.Errorf("chat.send 错误: %s", errMsg)
			}

			var chatResp ChatSendResponse
			if err := json.Unmarshal(resp.Payload, &chatResp); err != nil {
				return fullContent, fmt.Errorf("解析聊天响应失败: %w", err)
			}

			if chatResp.Content != "" {
				return chatResp.Content, nil
			}
			return fullContent, nil
		}
	}
}

// ExtractMessageContent 从任意类型的消息体中提取文本内容
func ExtractMessageContent(msg interface{}) string {
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

// buildWSURL 将 HTTP/HTTPS URL 转换为 WS/WSS URL 并追加 /ws 路径
func buildWSURL(baseURL string) string {
	wsURL := baseURL
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
	return wsURL
}
