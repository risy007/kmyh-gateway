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
	bindInfoRaw, ok := req.Extra["bind_info"]
	if !ok {
		return nil, fmt.Errorf("bind_info not found in request extra")
	}

	var bindInfo *kmyhconfig.CustomerBindInfo
	switch v := bindInfoRaw.(type) {
	case *kmyhconfig.CustomerBindInfo:
		bindInfo = v
	case kmyhconfig.CustomerBindInfo:
		bindInfo = &v
	default:
		return nil, fmt.Errorf("bind_info has unsupported type %T", bindInfoRaw)
	}

	// Use AgentType from bind_info directly
	agentType := bindInfo.AgentType
	if agentType == "" {
		agentType = "goclaw" // default
	}

	var agentConfigJSON json.RawMessage
	if agentType == "goclaw" {
		if bindInfo.AgentConfig != nil && bindInfo.AgentConfig.Goclaw != nil {
			gc := bindInfo.AgentConfig.Goclaw
			agentConfigJSON, _ = json.Marshal(map[string]string{
				"base_url": gc.BaseURL,
				"api_key":  gc.APIKey,
				"agent_id": gc.AgentID,
			})
		} else {
			// fallback to flat fields
			agentConfigJSON, _ = json.Marshal(map[string]string{
				"base_url": bindInfo.BaseURL,
				"api_key":  bindInfo.APIKey,
				"agent_id": bindInfo.AgentID,
			})
		}
	} else if bindInfo.AgentConfig != nil {
		agentConfigJSON, _ = json.Marshal(bindInfo.AgentConfig)
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
			"bind_info":    bindInfo,
		},
	}

	return processor.Process(ctx, agentReq)
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
