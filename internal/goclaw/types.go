package goclaw

import (
	"encoding/json"
	"time"
)

// ============ 通用类型 ============

type TenantData struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Slug      string          `json:"slug"`
	Status    string          `json:"status"`
	Settings  json.RawMessage `json:"settings,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

type AgentData struct {
	ID                  string          `json:"id"`
	TenantID            string          `json:"tenant_id"`
	AgentKey            string          `json:"agent_key"`
	DisplayName         string          `json:"display_name,omitempty"`
	Frontmatter         string          `json:"frontmatter,omitempty"`
	OwnerID             string          `json:"owner_id"`
	Provider            string          `json:"provider"`
	Model               string          `json:"model"`
	ContextWindow       int             `json:"context_window"`
	MaxToolIterations   int             `json:"max_tool_iterations"`
	Workspace           string          `json:"workspace"`
	RestrictToWorkspace bool            `json:"restrict_to_workspace"`
	AgentType           string          `json:"agent_type"`
	IsDefault           bool            `json:"is_default"`
	Status              string          `json:"status"`
	BudgetMonthlyCents  *int            `json:"budget_monthly_cents,omitempty"`
	ToolsConfig         json.RawMessage `json:"tools_config,omitempty"`
	SandboxConfig       json.RawMessage `json:"sandbox_config,omitempty"`
	SubagentsConfig     json.RawMessage `json:"subagents_config,omitempty"`
	MemoryConfig        json.RawMessage `json:"memory_config,omitempty"`
	CompactionConfig    json.RawMessage `json:"compaction_config,omitempty"`
	ContextPruning      json.RawMessage `json:"context_pruning,omitempty"`
	OtherConfig         json.RawMessage `json:"other_config,omitempty"`
}

type ProviderData struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	DisplayName  string          `json:"display_name,omitempty"`
	ProviderType string          `json:"provider_type"`
	APIBase      string          `json:"api_base,omitempty"`
	Enabled      bool            `json:"enabled"`
	Settings     json.RawMessage `json:"settings,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
	TenantID     string          `json:"tenant_id,omitempty"`
}

type ModelData struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name,omitempty"`
}

// ============ WebSocket 帧类型 ============

type WSFrameType string

const (
	WSFrameRequest  WSFrameType = "req"
	WSFrameResponse WSFrameType = "res"
	WSFrameEvent    WSFrameType = "event"
)

type WSRequestFrame struct {
	Type   WSFrameType `json:"type"`
	ID     string      `json:"id"`
	Method string      `json:"method"`
	Params interface{} `json:"params,omitempty"`
}

type WSResponseFrame struct {
	Type    WSFrameType     `json:"type"`
	ID      string          `json:"id"`
	OK      bool            `json:"ok"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Error   *WSErrorFrame   `json:"error,omitempty"`
}

type WSErrorFrame struct {
	Code         string      `json:"code"`
	Message      string      `json:"message"`
	Details      interface{} `json:"details,omitempty"`
	Retryable    bool        `json:"retryable,omitempty"`
	RetryAfterMs int         `json:"retryAfterMs,omitempty"`
}

type WSEventFrame struct {
	Type    WSFrameType     `json:"type"`
	Event   string          `json:"event"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Seq     int64           `json:"seq,omitempty"`
}

// ============ 连接相关 ============

type WSGatewayInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type WSConnectParams struct {
	Token      string `json:"token,omitempty"`
	UserID     string `json:"user_id,omitempty"`
	SenderID   string `json:"sender_id,omitempty"`
	Locale     string `json:"locale,omitempty"`
	TenantHint string `json:"tenant_hint,omitempty"`
	TenantID   string `json:"tenant_id,omitempty"`
}

type WSConnectResponse struct {
	Protocol   int           `json:"protocol"`
	Role       string        `json:"role"`
	UserID     string        `json:"user_id"`
	TenantID   string        `json:"tenant_id"`
	IsOwner    bool          `json:"is_owner"`
	TenantName string        `json:"tenant_name,omitempty"`
	TenantSlug string        `json:"tenant_slug,omitempty"`
	Server     WSGatewayInfo `json:"server"`
}

// ============ 聊天相关 ============

type ChatSendParams struct {
	Message     interface{} `json:"message"`
	AgentID     string      `json:"agentId,omitempty"`
	SessionKey  string      `json:"sessionKey,omitempty"`
	Stream      bool        `json:"stream,omitempty"`
	SystemPrompt string     `json:"systemPrompt,omitempty"`
}

type ChatSendResponse struct {
	Content string  `json:"content,omitempty"`
	Usage   *Usage  `json:"usage,omitempty"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
}

// ============ 流式事件 ============

type ChunkEvent struct {
	Content string `json:"content"`
}

type ToolCallEvent struct {
	Name string `json:"name"`
	ID   string `json:"id"`
	Args string `json:"args,omitempty"`
}

type ToolResultEvent struct {
	ID     string `json:"id"`
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

type RunStartedEvent struct {
	RunID string `json:"runId"`
}

type RunCompletedEvent struct {
	RunID   string `json:"runId"`
	Summary string `json:"summary,omitempty"`
}

// ============ Session 相关 ============

type SessionInfo struct {
	SessionKey string `json:"session_key"`
	TenantID   string `json:"tenant_id"`
	AgentID    string `json:"agent_id"`
	CreatedAt  string `json:"created_at,omitempty"`
}

type SessionListResponse struct {
	Sessions []SessionInfo `json:"sessions"`
}

// ============ 事件名称常量 ============

const (
	EventChunk        = "chunk"
	EventToolCall     = "tool.call"
	EventToolResult   = "tool.result"
	EventRunStarted   = "run.started"
	EventRunCompleted = "run.completed"
	EventError        = "error"
	EventMessage      = "message"
)
