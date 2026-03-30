package agent

import "context"

type AgentProcessor interface {
	Type() string
	Process(ctx context.Context, req *AgentRequest) (*AgentResponse, error)
}

type AgentRequest struct {
	CustomerID    string
	Message       interface{}
	Channel       string
	ChannelUserID string
	Extra         map[string]interface{}
}

type AgentResponse struct {
	Content   string
	Type      string
	Meta      map[string]interface{}
	Error     error
	Recipients []Recipient
}

type Recipient struct {
	ChannelType string
	UserID      string
	ChannelID   string
}
