package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"github.com/redis/go-redis/v9"
	kmyhconfig "github.com/risy007/kmyh-config"
	"github.com/risy007/kmyh-gateway/agent"
	"github.com/risy007/kmyh-gateway/internal/aibot"
	"github.com/risy007/kmyh-gateway/internal/httphandler"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

type (
	FeishuGateway struct {
		mu            sync.RWMutex
		log           *zap.SugaredLogger
		cfg           kmyhconfig.FeishuConfig
		aibotClient   *aibot.AIBotClient
		agentRegistry *agent.Registry
		channelMgr    *aibot.ChannelManager
		httpHandler   *httphandler.HttpHandler
		routerV1      *echo.Group

		channels map[string]*FeishuChannel

		redisClient *redis.Client
		redisPrefix string

		msgDedupTimeout time.Duration

		workerNum int
		wg        sync.WaitGroup
		ctx       context.Context
		cancel    context.CancelFunc
	}

	MessageTask struct {
		TenantSlug string
		Event      *larkim.P2MessageReceiveV1
		MsgID      string
		UserID     string
		Channel    *aibot.ChannelInfo
	}

	FeishuChannel struct {
		client     *lark.Client
		dispatcher *dispatcher.EventDispatcher
		config     FeishuChannelConfig
	}

	FeishuChannelConfig struct {
		AppID             string
		AppSecret         string
		CallbackType      string
		EncryptKey        string
		VerificationToken string
	}
)

func NewFeishuGateway(logger *zap.Logger, appConfig *kmyhconfig.AppConfig, cfgMgr *kmyhconfig.ConfigManager, httpHandler *httphandler.HttpHandler, channelMgr *aibot.ChannelManager) (*FeishuGateway, error) {
	log := logger.With(zap.Namespace("[FeishuGateway]")).Sugar()

	feishuCfgGroup, err := cfgMgr.GetGroup(appConfig.AppName, appConfig.Env, "feishu")
	if err != nil {
		return nil, err
	}

	var cfg kmyhconfig.FeishuConfig
	if err := feishuCfgGroup.Unmarshal(&cfg); err != nil {
		log.Warn("解析 feishu 配置失败", zap.Error(err))
	}

	ctx, cancel := context.WithCancel(context.Background())
	gw := &FeishuGateway{
		log:             log,
		cfg:             cfg,
		channelMgr:      channelMgr,
		channels:        make(map[string]*FeishuChannel),
		msgDedupTimeout: 5 * time.Minute,
		workerNum:       5,
		ctx:             ctx,
		cancel:          cancel,
	}

	redisCfgGroup, err := cfgMgr.GetGroup(appConfig.AppName, appConfig.Env, "redis")
	if err != nil {
		redisCfgGroup, err = cfgMgr.GetGroup("common", appConfig.Env, "redis")
	}
	if err == nil {
		var redisCfg kmyhconfig.RedisConfig
		if err := redisCfgGroup.Unmarshal(&redisCfg); err == nil {
			rdb := redis.NewClient(
				&redis.Options{
					Addr:     redisCfg.Addr(),
					Password: redisCfg.Password,
					DB:       redisCfg.MainDBId,
				})

			if err := rdb.Ping(ctx).Err(); err == nil {
				gw.redisClient = rdb
				gw.redisPrefix = redisCfg.KeyPrefix
				log.Info("Redis 连接成功", zap.String("addr", redisCfg.Addr()))
			} else {
				log.Warn("Redis 连接失败，使用内存模式", zap.Error(err))
				rdb.Close()
			}
		} else {
			log.Warn("解析 Redis 配置失败，使用内存模式", zap.Error(err))
		}
	} else {
		log.Warn("获取 Redis 配置失败，使用内存模式", zap.Error(err))
	}

	for i := 0; i < gw.workerNum; i++ {
		gw.wg.Add(1)
		go gw.worker(i)
	}

	if httpHandler != nil {
		gw.httpHandler = httpHandler
		gw.routerV1 = httpHandler.RouterV1.Group("/feishu")
		gw.registerRoutes()
	}

	feishuCfgGroup.OnChange(func() {
		var newCfg kmyhconfig.FeishuConfig
		if err := feishuCfgGroup.Unmarshal(&newCfg); err != nil {
			log.Error("解析新 feishu 配置失败", zap.Error(err))
			return
		}
		gw.mu.Lock()
		gw.cfg = newCfg
		gw.mu.Unlock()
	})

	return gw, nil
}

func (gw *FeishuGateway) registerRoutes() {
	if gw.routerV1 == nil {
		return
	}

	gw.routerV1.POST("/:tenant_slug", gw.webhookHandler)
}

func (gw *FeishuGateway) webhookHandler(c echo.Context) error {
	tenantSlug := c.Param("tenant_slug")
	if tenantSlug == "" {
		return c.String(http.StatusBadRequest, "tenant_slug is required")
	}

	gw.mu.RLock()
	ch, ok := gw.channels[tenantSlug]
	gw.mu.RUnlock()

	if !ok {
		gw.log.Warn("[FeishuGateway] 未找到渠道处理器", zap.String("tenant_slug", tenantSlug))
		return c.String(http.StatusNotFound, "channel not found")
	}

	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return c.String(http.StatusBadRequest, "Bad Request")
	}

	req := &larkevent.EventReq{
		Header:     c.Request().Header,
		Body:       body,
		RequestURI: c.Request().URL.Path,
	}

	resp := ch.dispatcher.Handle(context.Background(), req)
	if resp == nil {
		return c.String(http.StatusOK, "success")
	}

	for k, v := range resp.Header {
		for _, val := range v {
			c.Response().Header().Add(k, val)
		}
	}
	return c.Blob(resp.StatusCode, resp.Header.Get("Content-Type"), resp.Body)
}

var Module = fx.Module("gateway.feishu",
	fx.Provide(NewFeishuGateway),
	fx.Invoke(func(cm *aibot.ChannelManager, gw *FeishuGateway, aibotClient *aibot.AIBotClient, ar *agent.Registry) error {
		gw.SetAIBotClient(aibotClient)
		gw.SetAgentRegistry(ar)
		cm.Register(gw)
		return nil
	}),
)

func (gw *FeishuGateway) SetAIBotClient(client *aibot.AIBotClient) {
	gw.aibotClient = client
}

func (gw *FeishuGateway) SetAgentRegistry(ar *agent.Registry) {
	gw.agentRegistry = ar
}

func (gw *FeishuGateway) Start() error {
	gw.log.Info("[FeishuGateway] 启动")
	return nil
}

func (gw *FeishuGateway) Stop() error {
	gw.log.Info("[FeishuGateway] 停止")
	gw.cancel()
	gw.wg.Wait()
	if gw.redisClient != nil {
		if err := gw.redisClient.Close(); err != nil {
			gw.log.Error("[FeishuGateway] Redis 连接关闭失败", zap.Error(err))
		}
	}
	return nil
}

func (gw *FeishuGateway) worker(id int) {
	defer gw.wg.Done()
	gw.log.Infof("[FeishuGateway] 工作协程 %d 启动", id)

	for {
		select {
		case <-gw.ctx.Done():
			gw.log.Infof("[FeishuGateway] 工作协程 %d 退出", id)
			return
		default:
		}

		if gw.redisClient == nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		result, err := gw.redisClient.BRPop(gw.ctx, 5*time.Second, gw.queueKey()).Result()
		if err != nil {
			if err != redis.Nil {
				gw.log.Error("[FeishuGateway] Redis 队列读取失败", zap.Error(err))
			}
			continue
		}

		if len(result) < 2 {
			continue
		}

		var task MessageTask
		if err := json.Unmarshal([]byte(result[1]), &task); err != nil {
			gw.log.Error("[FeishuGateway] 任务反序列化失败", zap.Error(err))
			continue
		}

		gw.processMessageTask(&task)
	}
}

func (gw *FeishuGateway) processMessageTask(task *MessageTask) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	msg := task.Event.Event.Message

	var messageContent string
	if msg.Content != nil {
		messageContent = *msg.Content
	}

	agentMsg := map[string]interface{}{
		"tenant_slug": task.TenantSlug,
		"msg_id":      task.MsgID,
		"chat_type":   msg.ChatType,
		"user_id":     task.UserID,
		"content":     messageContent,
	}

	bindInfo, err := gw.aibotClient.GetCustomerBindInfo(ctx, &kmyhconfig.GetCustomerBindInfoRequest{
		Channel:       "feishu",
		ChannelUserID: task.UserID,
		TenantSlug:    task.TenantSlug,
	})
	if err != nil {
		gw.log.Error("[FeishuGateway] 异步获取客户绑定信息失败", zap.Error(err))
		return
	}

	if !bindInfo.Found || bindInfo.Info == nil || bindInfo.Info.UnifiedUserID == "" {
		gw.log.Warn("[FeishuGateway] 异步处理：未找到客户绑定信息",
			zap.String("channel_user_id", task.UserID))
		gw.sendTextMessage(ctx, task.TenantSlug, task.UserID, "您的账号尚未与系统用户关联，请联系管理员在租户详情页完成统一用户绑定后再使用。")
		return
	}

	var agentConfigRaw json.RawMessage
	if bindInfo.Info.AgentConfig != nil && bindInfo.Info.AgentConfig.Goclaw != nil {
		agentConfigRaw, _ = json.Marshal(bindInfo.Info.AgentConfig.Goclaw)
	}

	agentResp, err := gw.agentRegistry.Process(ctx, &agent.AgentRequest{
		CustomerID:    bindInfo.Info.TenantID,
		Message:       agentMsg,
		Channel:       "feishu",
		ChannelUserID: task.UserID,
		Extra: map[string]interface{}{
			"bind_info":    bindInfo.Info,
			"agent_config": agentConfigRaw,
		},
	})
	if err != nil {
		gw.log.Error("[FeishuGateway] 异步Agent处理失败", zap.Error(err))
		gw.sendTextMessage(ctx, task.TenantSlug, task.UserID, "服务暂时繁忙，请稍后重试。")
		return
	}

	if err := gw.sendAgentResponse(ctx, task.TenantSlug, task.UserID, agentResp); err != nil {
		gw.log.Error("[FeishuGateway] 异步发送Agent响应失败", zap.Error(err))
	}
}

func (gw *FeishuGateway) GetChannelType() string {
	return "feishu"
}

func (gw *FeishuGateway) InitChannel(channel *aibot.ChannelInfo) error {
	gw.log.Info("[FeishuGateway] 初始化渠道",
		zap.String("channel_id", channel.ChannelID),
		zap.String("tenant_id", channel.TenantID),
		zap.String("tenant_slug", channel.TenantSlug))

	if channel.Config == nil {
		return nil
	}

	var rawCfg struct {
		AppID             string `json:"app_id"`
		AppSecret         string `json:"app_secret"`
		CallbackType      string `json:"callback_type"`
		EncryptKey        string `json:"encrypt_key,omitempty"`
		VerificationToken string `json:"verification_token,omitempty"`
		WebhookPath       string `json:"webhook_path,omitempty"`
	}

	if err := json.Unmarshal(channel.Config, &rawCfg); err != nil {
		return fmt.Errorf("解析飞书配置失败: %w", err)
	}

	if rawCfg.AppID == "" || rawCfg.AppSecret == "" {
		return fmt.Errorf("app_id or app_secret is empty")
	}

	channelCfg := FeishuChannelConfig{
		AppID:             rawCfg.AppID,
		AppSecret:         rawCfg.AppSecret,
		CallbackType:      rawCfg.CallbackType,
		EncryptKey:        rawCfg.EncryptKey,
		VerificationToken: rawCfg.VerificationToken,
	}

	dp := dispatcher.NewEventDispatcher(rawCfg.VerificationToken, rawCfg.EncryptKey)

	dp.OnP2MessageReceiveV1(func(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
		return gw.handleMessageReceive(ctx, channel.TenantSlug, event)
	})

	gw.mu.Lock()
	gw.channels[channel.TenantSlug] = &FeishuChannel{
		client:     lark.NewClient(rawCfg.AppID, rawCfg.AppSecret),
		dispatcher: dp,
		config:     channelCfg,
	}
	gw.mu.Unlock()

	gw.log.Info("[FeishuGateway] 渠道初始化完成",
		zap.String("channel_id", channel.ChannelID),
		zap.String("tenant_slug", channel.TenantSlug))

	return nil
}

func (gw *FeishuGateway) handleMessageReceive(ctx context.Context, tenantSlug string, event *larkim.P2MessageReceiveV1) error {
	if event == nil || event.Event == nil || event.Event.Message == nil {
		return nil
	}

	msg := event.Event.Message
	sender := event.Event.Sender

	gw.log.Info("[FeishuGateway] 收到飞书消息",
		zap.String("tenant_slug", tenantSlug))

	var userID string
	var msgID, chatType, msgType string
	if msg.MessageId != nil {
		msgID = *msg.MessageId
	}
	if msg.ChatType != nil {
		chatType = *msg.ChatType
	}
	if msg.MessageType != nil {
		msgType = *msg.MessageType
	}
	if sender != nil && sender.SenderId != nil && sender.SenderId.OpenId != nil {
		userID = *sender.SenderId.OpenId
	}

	gw.log.Info("[FeishuGateway] 收到飞书消息",
		zap.String("tenant_slug", tenantSlug),
		zap.String("msg_id", msgID),
		zap.String("chat_type", chatType),
		zap.String("msg_type", msgType))

	if msgID != "" && gw.isDuplicateMessage(ctx, msgID) {
		gw.log.Info("[FeishuGateway] 消息已处理，跳过重复消息",
			zap.String("msg_id", msgID),
			zap.String("tenant_slug", tenantSlug))
		return nil
	}

	if gw.channelMgr != nil {
		channel := gw.channelMgr.GetChannelBySlug(tenantSlug)
		if channel != nil && channel.Status != aibot.ChannelStatusActive {
			gw.log.Warn("[FeishuGateway] 渠道已暂停或禁用",
				zap.String("tenant_slug", tenantSlug),
				zap.String("status", string(channel.Status)))
			return gw.sendSuspendedResponse(userID, tenantSlug, channel.Status)
		}
	}

	if userID == "" {
		gw.log.Warn("[FeishuGateway] 无法提取用户ID")
		return nil
	}

	gw.markMessageProcessed(ctx, msgID)

	task := &MessageTask{
		TenantSlug: tenantSlug,
		Event:      event,
		MsgID:      msgID,
		UserID:     userID,
	}

	taskData, err := json.Marshal(task)
	if err != nil {
		gw.log.Error("[FeishuGateway] 任务序列化失败", zap.Error(err))
		return gw.processMessageSync(ctx, tenantSlug, event, msgID, userID)
	}

	if gw.redisClient != nil {
		if err := gw.redisClient.LPush(ctx, gw.queueKey(), taskData).Err(); err != nil {
			gw.log.Error("[FeishuGateway] Redis 队列写入失败", zap.Error(err))
			return gw.processMessageSync(ctx, tenantSlug, event, msgID, userID)
		}
		gw.log.Info("[FeishuGateway] 消息已加入 Redis 队列",
			zap.String("msg_id", msgID),
			zap.String("tenant_slug", tenantSlug),
			zap.String("user_id", userID))
	} else {
		return gw.processMessageSync(ctx, tenantSlug, event, msgID, userID)
	}

	return nil
}

func (gw *FeishuGateway) processMessageSync(ctx context.Context, tenantSlug string, event *larkim.P2MessageReceiveV1, msgID, userID string) error {
	msg := event.Event.Message

	bindInfo, err := gw.aibotClient.GetCustomerBindInfo(ctx, &kmyhconfig.GetCustomerBindInfoRequest{
		Channel:       "feishu",
		ChannelUserID: userID,
		TenantSlug:    tenantSlug,
	})
	if err != nil {
		gw.log.Error("[FeishuGateway] 同步获取客户绑定信息失败", zap.Error(err))
		return err
	}

	if !bindInfo.Found || bindInfo.Info == nil || bindInfo.Info.UnifiedUserID == "" {
		gw.log.Warn("[FeishuGateway] 未找到客户绑定信息或尚未关联统一用户",
			zap.String("channel_user_id", userID))
		return gw.sendTextMessage(ctx, tenantSlug, userID, "您的账号尚未与系统用户关联，请联系管理员在租户详情页完成统一用户绑定后再使用。")
	}

	messageContent := ""
	if msg.Content != nil {
		messageContent = *msg.Content
	}

	agentMsg := map[string]interface{}{
		"tenant_slug": tenantSlug,
		"msg_id":      msg.MessageId,
		"msg_type":    msg.MessageType,
		"chat_type":   msg.ChatType,
		"user_id":     userID,
		"content":     messageContent,
	}

	var agentConfigRaw json.RawMessage
	if bindInfo.Info.AgentConfig != nil && bindInfo.Info.AgentConfig.Goclaw != nil {
		agentConfigRaw, _ = json.Marshal(bindInfo.Info.AgentConfig.Goclaw)
	}

	agentResp, err := gw.agentRegistry.Process(ctx, &agent.AgentRequest{
		CustomerID:    bindInfo.Info.TenantID,
		Message:       agentMsg,
		Channel:       "feishu",
		ChannelUserID: userID,
		Extra: map[string]interface{}{
			"bind_info":    bindInfo.Info,
			"agent_config": agentConfigRaw,
		},
	})
	if err != nil {
		gw.log.Error("[FeishuGateway] Agent处理失败", zap.Error(err))
		return err
	}

	return gw.sendAgentResponse(ctx, tenantSlug, userID, agentResp)
}

func (gw *FeishuGateway) UpdateChannel(channel *aibot.ChannelInfo) error {
	gw.log.Info("[FeishuGateway] 更新渠道",
		zap.String("channel_id", channel.ChannelID))

	if err := gw.DeleteChannel(channel.ChannelID); err != nil {
		gw.log.Warn("[FeishuGateway] 删除旧渠道失败", zap.Error(err))
	}

	return gw.InitChannel(channel)
}

func (gw *FeishuGateway) DeleteChannel(channelID string) error {
	gw.log.Info("[FeishuGateway] 删除渠道", zap.String("channel_id", channelID))

	gw.mu.Lock()
	defer gw.mu.Unlock()

	var tenantSlug string
	for slug, ch := range gw.channels {
		_ = ch
		if slug == channelID || strings.HasSuffix(slug, channelID) {
			tenantSlug = slug
			break
		}
	}

	if tenantSlug != "" {
		delete(gw.channels, tenantSlug)
	}

	return nil
}

func (gw *FeishuGateway) PauseChannel(channelID string) error {
	gw.log.Info("[FeishuGateway] 暂停渠道", zap.String("channel_id", channelID))
	return nil
}

func (gw *FeishuGateway) HandleMessage(msg interface{}) error {
	gw.log.Info("[FeishuGateway] 收到飞书消息", zap.Any("msg", msg))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	userID := gw.extractUserID(msg)
	if userID == "" {
		gw.log.Warn("[FeishuGateway] 无法提取用户ID")
		return nil
	}

	var tenantSlug string
	gw.mu.RLock()
	for slug := range gw.channels {
		tenantSlug = slug
		break
	}
	gw.mu.RUnlock()

	bindInfo, err := gw.aibotClient.GetCustomerBindInfo(ctx, &kmyhconfig.GetCustomerBindInfoRequest{
		Channel:       "feishu",
		ChannelUserID: userID,
		TenantSlug:    tenantSlug,
	})
	if err != nil {
		gw.log.Error("[FeishuGateway] 获取客户绑定信息失败", zap.Error(err))
		return err
	}

	if !bindInfo.Found || bindInfo.Info == nil || bindInfo.Info.UnifiedUserID == "" {
		gw.log.Warn("[FeishuGateway] 未找到客户绑定信息或尚未关联统一用户",
			zap.String("channel_user_id", userID))
		return nil
	}

	agentResp, err := gw.agentRegistry.Process(ctx, &agent.AgentRequest{
		CustomerID:    bindInfo.Info.TenantID,
		Message:       msg,
		Channel:       "feishu",
		ChannelUserID: userID,
		Extra: map[string]interface{}{
			"bind_info": bindInfo.Info,
		},
	})
	if err != nil {
		gw.log.Error("[FeishuGateway] Agent处理失败", zap.Error(err))
		return err
	}

	gw.log.Info("[FeishuGateway] 处理完成")
	return gw.sendAgentResponse(ctx, bindInfo.Info.TenantSlug, userID, agentResp)
}

func (gw *FeishuGateway) dedupKey(msgID string) string {
	return fmt.Sprintf("%s:feishu:dedup:%s", gw.redisPrefix, msgID)
}

func (gw *FeishuGateway) queueKey() string {
	return fmt.Sprintf("%s:feishu:msgqueue", gw.redisPrefix)
}

func (gw *FeishuGateway) isDuplicateMessage(ctx context.Context, msgID string) bool {
	if msgID == "" || gw.redisClient == nil {
		return false
	}
	key := gw.dedupKey(msgID)
	exists, err := gw.redisClient.Exists(ctx, key).Result()
	if err != nil {
		gw.log.Warn("[FeishuGateway] Redis 去重检查失败", zap.Error(err))
		return false
	}
	return exists > 0
}

func (gw *FeishuGateway) markMessageProcessed(ctx context.Context, msgID string) {
	if msgID == "" || gw.redisClient == nil {
		return
	}
	key := gw.dedupKey(msgID)
	if err := gw.redisClient.Set(ctx, key, "1", gw.msgDedupTimeout).Err(); err != nil {
		gw.log.Warn("[FeishuGateway] Redis 去重标记失败", zap.Error(err))
	}
}

func (gw *FeishuGateway) GetCallbackType() string {
	gw.mu.RLock()
	defer gw.mu.RUnlock()
	if len(gw.channels) == 0 {
		return ""
	}
	for _, ch := range gw.channels {
		if ch.config.CallbackType != "" {
			return ch.config.CallbackType
		}
	}
	return ""
}

func (gw *FeishuGateway) sendTextMessage(ctx context.Context, tenantSlug, userID, text string) error {
	gw.mu.RLock()
	ch, ok := gw.channels[tenantSlug]
	gw.mu.RUnlock()

	if !ok || ch == nil || ch.client == nil {
		gw.log.Warn("[FeishuGateway] 无法发送文本消息，client未找到",
			zap.String("tenant_slug", tenantSlug))
		return nil
	}

	content, _ := json.Marshal(map[string]string{"text": text})
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("open_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(userID).
			MsgType("text").
			Content(string(content)).
			Build()).
		Build()

	_, err := ch.client.Im.V1.Message.Create(ctx, req)
	if err != nil {
		gw.log.Error("[FeishuGateway] 发送文本消息失败", zap.Error(err))
		return err
	}
	return nil
}

func (gw *FeishuGateway) sendAgentResponse(ctx context.Context, tenantSlug, userID string, resp *agent.AgentResponse) error {
	if resp == nil {
		return nil
	}

	gw.mu.RLock()
	ch, ok := gw.channels[tenantSlug]
	if !ok {
		for _, c := range gw.channels {
			ch = c
			ok = true
			break
		}
	}
	gw.mu.RUnlock()

	if !ok || ch == nil || ch.client == nil {
		gw.log.Warn("[FeishuGateway] 无法发送Agent响应，client未找到",
			zap.String("tenant_slug", tenantSlug))
		return nil
	}

	msgType := "text"
	var contentBytes []byte
	if resp.Type == "markdown" {
		msgType = "post"
		line := map[string]interface{}{"tag": "text", "text": resp.Content}
		postContent := map[string]interface{}{
			"zh_cn": map[string]interface{}{
				"title":   "",
				"content": [][]map[string]interface{}{{line}},
			},
		}
		contentBytes, _ = json.Marshal(postContent)
	} else {
		contentBytes, _ = json.Marshal(map[string]string{"text": resp.Content})
	}

	for _, recipient := range resp.Recipients {
		targetID := recipient.UserID
		if targetID == "" {
			targetID = userID
		}

		req := larkim.NewCreateMessageReqBuilder().
			ReceiveIdType("open_id").
			Body(larkim.NewCreateMessageReqBodyBuilder().
				ReceiveId(targetID).
				MsgType(msgType).
				Content(string(contentBytes)).
				Build()).
			Build()

		_, err := ch.client.Im.V1.Message.Create(ctx, req)
		if err != nil {
			gw.log.Error("[FeishuGateway] 发送Agent响应失败", zap.Error(err))
			return err
		}
	}

	return nil
}

func (gw *FeishuGateway) sendSuspendedResponse(userID, tenantSlug string, status aibot.ChannelStatus) error {
	var reason string
	switch status {
	case aibot.ChannelStatusPaused:
		reason = "服务已暂停"
	case aibot.ChannelStatusDisabled:
		reason = "服务已停用"
	default:
		reason = "服务暂不可用"
	}

	msg := fmt.Sprintf("%s，请联系管理员处理。", reason)
	return gw.sendTextMessage(context.Background(), tenantSlug, userID, msg)
}

func (gw *FeishuGateway) extractUserID(msg interface{}) string {
	data, err := json.Marshal(msg)
	if err != nil {
		return ""
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return ""
	}

	if sender, ok := raw["sender"].(map[string]interface{}); ok {
		if id, ok := sender["sender_id"].(map[string]interface{}); ok {
			if openID, ok := id["open_id"].(string); ok {
				return openID
			}
		}
	}

	if userID, ok := raw["user_id"].(string); ok {
		return userID
	}

	return ""
}
