package agent

import (
	"context"
	"encoding/json"
	"fmt"

	kmyhconfig "github.com/risy007/kmyh-config"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

type Registry struct {
	log    *zap.SugaredLogger
	agents map[string]AgentProcessor
}

type RegistryIn struct {
	fx.In
	Logger      *zap.Logger
	AppConfig   *kmyhconfig.AppConfig
	CfgMgr      *kmyhconfig.ConfigManager
	GoclawAgent *GoclawAgent
	DifyAgent   *DifyAgent
}

func NewRegistry(in RegistryIn) (*Registry, error) {
	log := in.Logger.With(zap.Namespace("[AgentRegistry]")).Sugar()

	agents := make(map[string]AgentProcessor)

	if in.GoclawAgent != nil {
		agents["goclaw"] = in.GoclawAgent
		log.Info("[NewRegistry] Goclaw agent 已注册")
	}

	if in.DifyAgent != nil {
		agents["dify"] = in.DifyAgent
		log.Info("[NewRegistry] Dify agent 已注册")
	}

	log.Info("[NewRegistry] AgentRegistry 初始化完成", zap.Int("agent_count", len(agents)))

	return &Registry{
		log:    log,
		agents: agents,
	}, nil
}

func (r *Registry) Process(ctx context.Context, req *AgentRequest) (*AgentResponse, error) {
	bindInfo, ok := req.Extra["bind_info"]
	if !ok {
		return nil, fmt.Errorf("bind_info not found in request extra")
	}

	var agentType string
	var agentConfigJSON json.RawMessage

	switch v := bindInfo.(type) {
	case *kmyhconfig.CustomerBindInfo:
		agentType = "goclaw"
		agentConfigJSON = r.buildGoclawConfig(v)
	case kmyhconfig.CustomerBindInfo:
		agentType = "goclaw"
		agentConfigJSON = r.buildGoclawConfig(&v)
	case map[string]interface{}:
		if at, ok := v["agent_type"].(string); ok {
			agentType = at
		} else {
			agentType = "goclaw"
		}
		if ac, ok := v["agent_config"].(json.RawMessage); ok {
			agentConfigJSON = ac
		} else if _, ok := v["base_url"].(string); ok {
			agentConfigJSON = r.buildGoclawConfigFromMap(v)
		}
	default:
		return nil, fmt.Errorf("bind_info has unsupported type %T", bindInfo)
	}

	if agentType == "" {
		agentType = "goclaw"
	}

	processor, exists := r.agents[agentType]
	if !exists {
		return nil, fmt.Errorf("agent type %s not registered", agentType)
	}

	agentReq := &AgentRequest{
		CustomerID:    req.CustomerID,
		Message:       req.Message,
		Channel:       req.Channel,
		ChannelUserID: req.ChannelUserID,
		Extra: map[string]interface{}{
			"agent_config": agentConfigJSON,
		},
	}

	return processor.Process(ctx, agentReq)
}

func (r *Registry) buildGoclawConfig(info *kmyhconfig.CustomerBindInfo) json.RawMessage {
	config := map[string]string{
		"base_url": info.BaseURL,
		"api_key":  info.APIKey,
		"agent_id": info.AgentID,
	}
	data, _ := json.Marshal(config)
	return data
}

func (r *Registry) buildGoclawConfigFromMap(m map[string]interface{}) json.RawMessage {
	config := make(map[string]string)
	if v, ok := m["base_url"].(string); ok {
		config["base_url"] = v
	}
	if v, ok := m["api_key"].(string); ok {
		config["api_key"] = v
	}
	if v, ok := m["agent_id"].(string); ok {
		config["agent_id"] = v
	}
	data, _ := json.Marshal(config)
	return data
}

func (r *Registry) Register(agentType string, processor AgentProcessor) {
	r.agents[agentType] = processor
	r.log.Info("[Register] 已注册 Agent", zap.String("type", agentType))
}

func (r *Registry) Get(agentType string) (AgentProcessor, bool) {
	processor, ok := r.agents[agentType]
	return processor, ok
}

func (r *Registry) ListTypes() []string {
	types := make([]string, 0, len(r.agents))
	for t := range r.agents {
		types = append(types, t)
	}
	return types
}
