package goclaw

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

const (
	wsProtocolVersion   = 3
	wsWriteDeadline     = 10 * time.Second
	wsReadDeadline      = 60 * time.Second
	wsPingInterval      = 30 * time.Second
	wsReconnectInterval = 5 * time.Second
	wsMaxReconnectTries = 3
)

// WSSubscription 事件订阅
type WSSubscription struct {
	ID      string
	Event   string
	Handler func(*WSEventFrame)
}

// WSClient 是 WebSocket 客户端，提供与 GoClaw Gateway 的 WebSocket 通信
type WSClient struct {
	baseURL   string
	apiToken  string
	logger    *zap.Logger
	conn      *websocket.Conn
	mu        sync.RWMutex
	subs      map[string]map[string]*WSSubscription
	subMu     sync.RWMutex
	subSeq    uint64
	pending   map[string]chan *WSResponseFrame
	pendingMu sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	connected atomic.Bool
	connInfo  atomic.Value
	writeChan chan *WSRequestFrame
	done      chan struct{}

	reconnecting   atomic.Bool
	reconnectTries int32
}

// NewWSClient 创建一个新的 WebSocket 客户端
func NewWSClient(baseURL, apiToken string, logger *zap.Logger) *WSClient {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &WSClient{
		baseURL:  baseURL,
		apiToken: apiToken,
		logger:   logger.With(zap.String("component", "GoclawWSClient")),
		subs:     make(map[string]map[string]*WSSubscription),
		pending:  make(map[string]chan *WSResponseFrame),
		done:     make(chan struct{}),
	}
}

// buildWSURL 构建 WebSocket URL
func (c *WSClient) buildWSURL() string {
	wsURL := c.baseURL
	if len(wsURL) > 7 && wsURL[:7] == "http://" {
		wsURL = "ws://" + wsURL[7:]
	} else if len(wsURL) > 8 && wsURL[:8] == "https://" {
		wsURL = "wss://" + wsURL[8:]
	}
	if len(wsURL) > 0 && wsURL[len(wsURL)-1] == '/' {
		wsURL = wsURL[:len(wsURL)-1]
	}
	return wsURL + "/ws"
}

// Connect 连接到 GoClaw WebSocket Gateway
func (c *WSClient) Connect(ctx context.Context, params *WSConnectParams) (*WSConnectResponse, error) {
	if c.connected.Load() {
		if info := c.GetConnInfo(); info != nil {
			return info, nil
		}
	}

	wsURL := c.buildWSURL()
	c.logger.Debug("Connecting to WebSocket", zap.String("url", wsURL))

	header := http.Header{}
	header.Set("Authorization", fmt.Sprintf("Bearer %s", c.apiToken))
	header.Set("X-GoClaw-User-Id", "system")

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		return nil, fmt.Errorf("websocket dial failed: %w", err)
	}

	c.mu.Lock()
	c.conn = conn
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.writeChan = make(chan *WSRequestFrame, 256)
	c.mu.Unlock()

	go c.readPump()
	go c.writePump()

	reqID := uuid.NewString()
	req := &WSRequestFrame{
		Type:   WSFrameRequest,
		ID:     reqID,
		Method: "connect",
		Params: params,
	}

	resp, err := c.doRequest(c.ctx, req)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("connect failed: %w", err)
	}

	if !resp.OK {
		c.Close()
		errMsg := "unknown error"
		if resp.Error != nil {
			errMsg = fmt.Sprintf("%s - %s", resp.Error.Code, resp.Error.Message)
		}
		return nil, fmt.Errorf("connect rejected: %s", errMsg)
	}

	var connResp WSConnectResponse
	if err := json.Unmarshal(resp.Payload, &connResp); err != nil {
		c.Close()
		return nil, fmt.Errorf("parse connect response failed: %w", err)
	}

	c.connInfo.Store(&connResp)
	c.connected.Store(true)
	c.logger.Info("WebSocket connected",
		zap.String("role", connResp.Role),
		zap.String("tenant_id", connResp.TenantID))

	return &connResp, nil
}

// Reconnect 重新连接（带重试）
func (c *WSClient) Reconnect(ctx context.Context, params *WSConnectParams) error {
	if !c.reconnecting.CompareAndSwap(false, true) {
		c.logger.Debug("Reconnect already in progress")
		return nil
	}
	defer c.reconnecting.Store(false)

	c.logger.Info("Starting reconnection attempt", zap.Int32("attempt", c.reconnectTries+1))
	c.Close()

	backoff := time.Duration(1<<c.reconnectTries) * wsReconnectInterval
	if backoff > 30*time.Second {
		backoff = 30 * time.Second
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(backoff):
	}

	c.connected.Store(false)

	_, err := c.Connect(ctx, params)
	if err != nil {
		atomic.AddInt32(&c.reconnectTries, 1)
		if c.reconnectTries < wsMaxReconnectTries {
			c.logger.Warn("Reconnection failed, will retry",
				zap.Int32("tries", c.reconnectTries),
				zap.Error(err))
			return c.Reconnect(ctx, params)
		}
		return fmt.Errorf("reconnection failed after %d attempts: %w", wsMaxReconnectTries, err)
	}

	c.reconnectTries = 0
	c.logger.Info("Reconnection successful")
	return nil
}

// doRequest 发送请求并等待响应
func (c *WSClient) doRequest(ctx context.Context, req *WSRequestFrame) (*WSResponseFrame, error) {
	resultChan := make(chan *WSResponseFrame, 1)

	c.pendingMu.Lock()
	c.pending[req.ID] = resultChan
	c.pendingMu.Unlock()

	select {
	case c.writeChan <- req:
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, req.ID)
		c.pendingMu.Unlock()
		return nil, ctx.Err()
	case <-c.done:
		c.pendingMu.Lock()
		delete(c.pending, req.ID)
		c.pendingMu.Unlock()
		return nil, fmt.Errorf("connection closed")
	}

	select {
	case resp := <-resultChan:
		c.pendingMu.Lock()
		delete(c.pending, req.ID)
		c.pendingMu.Unlock()
		return resp, nil
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, req.ID)
		c.pendingMu.Unlock()
		return nil, ctx.Err()
	case <-c.done:
		c.pendingMu.Lock()
		delete(c.pending, req.ID)
		c.pendingMu.Unlock()
		return nil, fmt.Errorf("connection closed")
	}
}

// readPump 读取循环
func (c *WSClient) readPump() {
	defer close(c.done)

	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()

	if conn == nil {
		return
	}

	conn.SetReadLimit(512 * 1024)
	conn.SetReadDeadline(time.Now().Add(wsReadDeadline))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(wsReadDeadline))
		return nil
	})

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				c.logger.Warn("websocket read error", zap.Error(err))
			}
			c.connected.Store(false)
			return
		}

		conn.SetReadDeadline(time.Now().Add(wsReadDeadline))

		var raw struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			c.logger.Warn("parse frame type failed", zap.Error(err))
			continue
		}

		switch raw.Type {
		case string(WSFrameResponse):
			var resp WSResponseFrame
			if err := json.Unmarshal(data, &resp); err != nil {
				c.logger.Warn("parse response failed", zap.Error(err))
				continue
			}

			c.pendingMu.Lock()
			ch, ok := c.pending[resp.ID]
			if ok {
				select {
				case ch <- &resp:
				default:
				}
			}
			c.pendingMu.Unlock()

		case string(WSFrameEvent):
			var event WSEventFrame
			if err := json.Unmarshal(data, &event); err != nil {
				c.logger.Warn("parse event failed", zap.Error(err))
				continue
			}

			c.dispatchEvent(&event)
		}
	}
}

// dispatchEvent 分发事件到订阅者
func (c *WSClient) dispatchEvent(event *WSEventFrame) {
	c.subMu.RLock()
	defer c.subMu.RUnlock()

	subs := c.subs[event.Event]
	for _, sub := range subs {
		go sub.Handler(event)
	}
}

// writePump 写入循环
func (c *WSClient) writePump() {
	ticker := time.NewTicker(wsPingInterval)
	defer ticker.Stop()

	for {
		select {
		case req := <-c.writeChan:
			c.mu.RLock()
			conn := c.conn
			c.mu.RUnlock()
			if conn == nil {
				return
			}

			conn.SetWriteDeadline(time.Now().Add(wsWriteDeadline))
			if err := conn.WriteMessage(websocket.TextMessage, mustMarshal(req)); err != nil {
				c.logger.Error("write message failed", zap.Error(err))
				return
			}

		case <-ticker.C:
			c.mu.RLock()
			conn := c.conn
			c.mu.RUnlock()
			if conn == nil {
				return
			}

			conn.SetWriteDeadline(time.Now().Add(wsWriteDeadline))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				c.logger.Error("write ping failed", zap.Error(err))
				return
			}

		case <-c.done:
			return
		}
	}
}

// Call 调用 GoClaw 方法，返回原始 payload
func (c *WSClient) Call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	reqID := uuid.NewString()
	req := &WSRequestFrame{
		Type:   WSFrameRequest,
		ID:     reqID,
		Method: method,
		Params: params,
	}

	resp, err := c.doRequest(ctx, req)
	if err != nil {
		return nil, err
	}

	if !resp.OK {
		errMsg := "unknown error"
		if resp.Error != nil {
			errMsg = fmt.Sprintf("%s - %s", resp.Error.Code, resp.Error.Message)
		}
		return nil, fmt.Errorf("WS call %s failed: %s", method, errMsg)
	}

	return resp.Payload, nil
}

// CallFor 调用方法并将结果解析到 result
func (c *WSClient) CallFor(ctx context.Context, method string, params interface{}, result interface{}) error {
	payload, err := c.Call(ctx, method, params)
	if err != nil {
		return err
	}

	if result != nil && payload != nil {
		if err := json.Unmarshal(payload, result); err != nil {
			return fmt.Errorf("unmarshal result failed: %w", err)
		}
	}

	return nil
}

// Subscribe 订阅事件，返回取消订阅的函数
func (c *WSClient) Subscribe(event string, handler func(*WSEventFrame)) func() {
	subID := fmt.Sprintf("%s-%d", uuid.NewString(), atomic.AddUint64(&c.subSeq, 1))

	sub := &WSSubscription{
		ID:      subID,
		Event:   event,
		Handler: handler,
	}

	c.subMu.Lock()
	if c.subs[event] == nil {
		c.subs[event] = make(map[string]*WSSubscription)
	}
	c.subs[event][subID] = sub
	c.subMu.Unlock()

	return func() {
		c.subMu.Lock()
		delete(c.subs[event], subID)
		c.subMu.Unlock()
	}
}

// SubscribeOnce 订阅一次性事件，返回取消订阅的函数
func (c *WSClient) SubscribeOnce(event string, handler func(*WSEventFrame)) func() {
	type cancelInfo struct {
		cancel func()
	}
	var once atomic.Value
	once.Store(cancelInfo{cancel: nil})

	var onceHandler func(*WSEventFrame)
	onceHandler = func(e *WSEventFrame) {
		info := once.Load().(cancelInfo)
		if info.cancel != nil {
			info.cancel()
		}
		handler(e)
	}

	cancel := c.Subscribe(event, onceHandler)
	once.Store(cancelInfo{cancel: cancel})
	return cancel
}

// IsConnected 检查是否已连接
func (c *WSClient) IsConnected() bool {
	return c.connected.Load()
}

// GetConnInfo 获取连接信息
func (c *WSClient) GetConnInfo() *WSConnectResponse {
	if v := c.connInfo.Load(); v != nil {
		return v.(*WSConnectResponse)
	}
	return nil
}

// ConnInfo 获取连接信息（别名，保持兼容性）
func (c *WSClient) ConnInfo() *WSConnectResponse {
	return c.GetConnInfo()
}

// Close 关闭连接
func (c *WSClient) Close() error {
	if !c.connected.CompareAndSwap(true, false) {
		return nil
	}

	if c.cancel != nil {
		c.cancel()
	}

	c.pendingMu.Lock()
	for _, ch := range c.pending {
		close(ch)
	}
	c.pendingMu.Unlock()

	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()

	if conn != nil {
		conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		conn.Close()
	}

	return nil
}

// ============ 聊天相关方法 ============

// ChatSend 发送聊天消息（同步）
func (c *WSClient) ChatSend(ctx context.Context, params *ChatSendParams) (*ChatSendResponse, error) {
	var resp ChatSendResponse
	if err := c.CallFor(ctx, "chat.send", params, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ChatSendStream 发送聊天消息（流式），通过回调处理 chunk
func (c *WSClient) ChatSendStream(ctx context.Context, params *ChatSendParams, onChunk func(string)) error {
	contentChan := make(chan string, 100)
	errChan := make(chan error, 1)
	doneChan := make(chan struct{})

	unsubChunk := c.Subscribe(EventChunk, func(event *WSEventFrame) {
		var chunk ChunkEvent
		if err := json.Unmarshal(event.Payload, &chunk); err != nil {
			c.logger.Warn("parse chunk event failed", zap.Error(err))
			return
		}
		select {
		case contentChan <- chunk.Content:
		case <-ctx.Done():
		}
	})

	unsubDone := c.Subscribe(EventRunCompleted, func(event *WSEventFrame) {
		close(doneChan)
	})

	reqID := uuid.NewString()
	req := &WSRequestFrame{
		Type:   WSFrameRequest,
		ID:     reqID,
		Method: "chat.send",
		Params: params,
	}

	select {
	case c.writeChan <- req:
	case <-ctx.Done():
		unsubChunk()
		unsubDone()
		return ctx.Err()
	}

	var collectedContent string
	for {
		select {
		case <-ctx.Done():
			unsubChunk()
			unsubDone()
			return ctx.Err()
		case chunk := <-contentChan:
			collectedContent += chunk
			if onChunk != nil {
				onChunk(chunk)
			}
		case <-doneChan:
			unsubChunk()
			unsubDone()
			return nil
		case err := <-errChan:
			unsubChunk()
			unsubDone()
			return err
		}
	}
}

// ChatSendStreamWait 发送聊天消息（流式），返回完整内容
func (c *WSClient) ChatSendStreamWait(ctx context.Context, params *ChatSendParams) (string, error) {
	var content string
	err := c.ChatSendStream(ctx, params, func(chunk string) {
		content += chunk
	})
	return content, err
}

// ChatHistory 获取聊天历史
func (c *WSClient) ChatHistory(ctx context.Context, sessionKey string, limit int) (json.RawMessage, error) {
	params := map[string]interface{}{
		"session": sessionKey,
		"limit":   limit,
	}
	return c.Call(ctx, "chat.history", params)
}

// ChatAbort 中止聊天
func (c *WSClient) ChatAbort(ctx context.Context, sessionKey string) error {
	params := map[string]interface{}{
		"session": sessionKey,
	}
	_, err := c.Call(ctx, "chat.abort", params)
	return err
}

// ============ Session 相关方法 ============

// ListSessions 获取 session 列表
func (c *WSClient) ListSessions(ctx context.Context) (*SessionListResponse, error) {
	var result SessionListResponse
	if err := c.CallFor(ctx, "sessions.list", nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ============ 状态检查方法 ============

// GetHealth 获取健康状态
func (c *WSClient) GetHealth(ctx context.Context) (json.RawMessage, error) {
	return c.Call(ctx, "health", nil)
}

// GetStatus 获取状态
func (c *WSClient) GetStatus(ctx context.Context) (json.RawMessage, error) {
	return c.Call(ctx, "status", nil)
}

// Ping 发送 ping 并等待响应（健康检查）
func (c *WSClient) Ping(ctx context.Context) error {
	_, err := c.Call(ctx, "ping", nil)
	return err
}

// mustMarshal JSON 序列化（内部用）
func mustMarshal(v interface{}) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}
