package goclaw

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/allegro/bigcache/v3"
	"github.com/google/uuid"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

type (
	goclawInParams struct {
		fx.In
		Logger *zap.Logger
		Ctx    context.Context
	}

	GoclawConfig struct {
		CachePeriod        string            `json:"cache_period"`
		Timeout            string            `json:"timeout"`
		StreamChunkTimeout string            `json:"stream_chunk_timeout"`
		TotalTimeout       string            `json:"total_timeout"`
		MaxRetries         int               `json:"max_retries"`
		AckMessage         string            `json:"ack_message"`
		TimeoutMessage     string            `json:"timeout_message"`
		StreamOutput       bool              `json:"stream_output"`
		Bots               []GoclawBotConfig `json:"bots"`
	}

	GoclawBotConfig struct {
		UserID  string `json:"user_id"`
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
		AgentID string `json:"agent_id"`
	}

	MessageSender interface {
		SendTextMessage(toUserID, content string) error
		SendMarkdownMessage(toUserID, content string) error
	}

	GoclawService struct {
		mu            sync.RWMutex
		httpClient    *http.Client
		cfg           GoclawConfig
		botMap        map[string]GoclawBotConfig
		log           *zap.Logger
		cache         *bigcache.BigCache
		ctx           context.Context
		sender        MessageSender
		wsClientCache map[string]*WSClient
		wsClientMu    sync.RWMutex
	}
)

func NewGoclawService(in goclawInParams) (*GoclawService, error) {
	log := in.Logger.With(zap.Namespace("[Goclaw接口服务]"))

	defaultCfg := GoclawConfig{
		CachePeriod:        "5m",
		Timeout:            "60s",
		StreamChunkTimeout: "30s",
		TotalTimeout:       "300s",
		MaxRetries:         3,
		AckMessage:         "我已收到消息，正在处理中，请稍候...",
		TimeoutMessage:     "任务处理超时（超过 %v），请稍后重试或联系管理员。",
		StreamOutput:       false,
	}

	timeout, err := time.ParseDuration(defaultCfg.Timeout)
	if err != nil {
		timeout = 60 * time.Second
	}

	duration, err := time.ParseDuration(defaultCfg.CachePeriod)
	if err != nil {
		return nil, fmt.Errorf("解析 CachePeriod 失败: %w", err)
	}
	cacheCfg := bigcache.DefaultConfig(duration)
	cache, err := bigcache.New(in.Ctx, cacheCfg)
	if err != nil {
		return nil, fmt.Errorf("初始化 BigCache 失败: %w", err)
	}

	service := &GoclawService{
		httpClient:    &http.Client{Timeout: timeout},
		cfg:           defaultCfg,
		botMap:        make(map[string]GoclawBotConfig),
		log:           log,
		cache:         cache,
		ctx:           in.Ctx,
		wsClientCache: make(map[string]*WSClient),
	}

	log.Info("Goclaw 服务初始化完成")

	return service, nil
}

func (s *GoclawService) SetMessageSender(sender MessageSender) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sender = sender
}

func (s *GoclawService) UpdateConfig(cfg GoclawConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()

	timeout, _ := time.ParseDuration(cfg.Timeout)
	if timeout > 0 {
		s.httpClient.Timeout = timeout
	}

	s.cfg = cfg

	newBotMap := make(map[string]GoclawBotConfig)
	for _, bot := range cfg.Bots {
		newBotMap[bot.UserID] = bot
	}
	s.botMap = newBotMap

	s.wsClientMu.Lock()
	for _, client := range s.wsClientCache {
		client.Close()
	}
	s.wsClientCache = make(map[string]*WSClient)
	s.wsClientMu.Unlock()

	s.log.Info("Goclaw 配置已更新",
		zap.Int("bot_count", len(newBotMap)))
}

func (s *GoclawService) Start() error {
	s.log.Info("[Start] 启动 Goclaw 服务", zap.String("StartTime", time.Now().Format("2006-01-02 15:04:05")))
	<-s.ctx.Done()
	return nil
}

func (s *GoclawService) Stop() error {
	s.wsClientMu.Lock()
	defer s.wsClientMu.Unlock()
	for _, client := range s.wsClientCache {
		client.Close()
	}
	return nil
}

func (s *GoclawService) getBotConfig(userID string) (GoclawBotConfig, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.botMap == nil {
		return GoclawBotConfig{}, false
	}
	bot, ok := s.botMap[userID]
	if !ok {
		bot, ok = s.botMap["default"]
	}
	return bot, ok
}

func (s *GoclawService) getOrCreateWSClient(bot GoclawBotConfig) (*WSClient, error) {
	s.wsClientMu.RLock()
	client, exists := s.wsClientCache[bot.UserID]
	s.wsClientMu.RUnlock()

	if exists && client.IsConnected() {
		return client, nil
	}

	client = NewWSClient(bot.BaseURL, bot.APIKey, s.log)
	params := &WSConnectParams{
		Token:  bot.APIKey,
		UserID: bot.UserID,
	}
	if _, err := client.Connect(s.ctx, params); err != nil {
		return nil, fmt.Errorf("连接 GoClaw WebSocket 失败: %w", err)
	}

	s.wsClientMu.Lock()
	s.wsClientCache[bot.UserID] = client
	s.wsClientMu.Unlock()

	return client, nil
}

func (s *GoclawService) HandleChannelMessage(msg *ChannelMessage) error {
	s.log.Info("[HandleChannelMessage] 处理渠道消息",
		zap.String("channel", string(msg.Channel)),
		zap.String("sender_id", msg.SenderID))

	s.mu.RLock()
	cache := s.cache
	s.mu.RUnlock()

	msgKey := fmt.Sprintf("msg:%s", msg.MsgID)
	if _, err := cache.Get(msgKey); err == nil {
		s.log.Info("[HandleChannelMessage] 消息已处理过，跳过")
		return nil
	}
	cache.Set(msgKey, []byte("processed"))

	s.mu.RLock()
	ackMsg := s.cfg.AckMessage
	streamOutput := s.cfg.StreamOutput
	s.mu.RUnlock()

	if err := s.sendTextMessage(msg.SenderID, ackMsg); err != nil {
		s.log.Warn("[HandleChannelMessage] 发送确认消息失败", zap.Error(err))
	}

	var filePath string
	if len(msg.MediaFiles) > 0 && len(msg.MediaFiles[0].Base64Data) > 0 {
		filePath = s.handleMediaFile(msg)
	}

	messageContent := s.buildMessageContent(msg.Text, filePath)
	if filePath != "" {
		defer os.Remove(filePath)
	}

	if streamOutput {
		return s.handleStreamRequest(msg, messageContent)
	} else {
		return s.handleNonStreamRequest(msg, messageContent)
	}
}

func (s *GoclawService) handleMediaFile(msg *ChannelMessage) string {
	if len(msg.MediaFiles) == 0 {
		return ""
	}

	file := msg.MediaFiles[0]
	if file.Base64Data == "" {
		return ""
	}

	ext := ".bin"
	if file.Filename != "" {
		ext = filepath.Ext(file.Filename)
	}

	data, err := base64.StdEncoding.DecodeString(file.Base64Data)
	if err != nil {
		s.log.Warn("[handleMediaFile] Base64 解码失败", zap.Error(err))
		return ""
	}

	filePath := filepath.Join(os.TempDir(), uuid.NewString()+ext)
	if err := os.WriteFile(filePath, data, 0644); err != nil {
		s.log.Warn("[handleMediaFile] 文件写入失败", zap.Error(err))
		return ""
	}

	return filePath
}

func (s *GoclawService) buildMessageContent(prompt string, filePath string) interface{} {
	if filePath == "" {
		return prompt
	}

	fileExt := strings.ToLower(filepath.Ext(filePath))
	fileName := filepath.Base(filePath)
	isImage := isImageExt(fileExt)

	fileData, err := os.ReadFile(filePath)
	if err != nil {
		return prompt + "\n[附件: " + fileName + "]"
	}

	mimeType := getMimeFromExt(fileExt)
	base64Data := base64.StdEncoding.EncodeToString(fileData)

	var blocks []map[string]interface{}
	if prompt != "" {
		blocks = append(blocks, map[string]interface{}{"type": "text", "text": prompt})
	}

	if isImage {
		blocks = append(blocks, map[string]interface{}{
			"type": "image_url",
			"image_url": map[string]string{
				"url":    fmt.Sprintf("data:%s;base64,%s", mimeType, base64Data),
				"detail": "auto",
			},
		})
	} else {
		blocks = append(blocks, map[string]interface{}{
			"type": "file",
			"file": map[string]string{
				"filename":  fileName,
				"file_data": fmt.Sprintf("data:%s;base64,%s", mimeType, base64Data),
			},
		})
	}
	return blocks
}

func (s *GoclawService) handleNonStreamRequest(msg *ChannelMessage, content interface{}) error {
	bot, ok := s.getBotConfig(msg.SenderID)
	if !ok {
		s.log.Error("[handleNonStreamRequest] 未找到 bot 配置", zap.String("sender_id", msg.SenderID))
		s.sendTextMessage(msg.SenderID, "未配置 AI 机器人，请联系管理员")
		return fmt.Errorf("未找到 bot 配置")
	}

	client, err := s.getOrCreateWSClient(bot)
	if err != nil {
		s.log.Error("[handleNonStreamRequest] 获取 WebSocket 客户端失败", zap.Error(err))
		s.sendTextMessage(msg.SenderID, fmt.Sprintf("连接 AI 服务失败: %v", err))
		return err
	}

	ctx, cancel := context.WithTimeout(s.ctx, s.httpClient.Timeout)
	defer cancel()

	sessionKey := BuildSessionID(msg.Channel, msg.ChatType, msg.SenderOpenID)
	resp, err := client.ChatSend(ctx, &ChatSendParams{
		Message:    content,
		AgentID:    bot.AgentID,
		SessionKey: sessionKey,
	})
	if err != nil {
		s.log.Error("[handleNonStreamRequest] 发送消息失败", zap.Error(err))
		if ctx.Err() == context.DeadlineExceeded {
			s.mu.RLock()
			timeoutMsg := s.cfg.TimeoutMessage
			s.mu.RUnlock()
			s.sendTimeoutMessage(msg.SenderID, s.httpClient.Timeout, timeoutMsg)
		}
		return err
	}

	if resp != nil && resp.Content != "" {
		return s.sendMarkdownMessage(msg.SenderID, resp.Content)
	}
	return nil
}

func (s *GoclawService) handleStreamRequest(msg *ChannelMessage, content interface{}) error {
	bot, ok := s.getBotConfig(msg.SenderID)
	if !ok {
		s.log.Error("[handleStreamRequest] 未找到 bot 配置")
		return fmt.Errorf("未找到 bot 配置")
	}

	s.mu.RLock()
	chunkTimeout, _ := time.ParseDuration(s.cfg.StreamChunkTimeout)
	totalTimeout, _ := time.ParseDuration(s.cfg.TotalTimeout)
	maxRetries := s.cfg.MaxRetries
	s.mu.RUnlock()

	totalCtx, totalCancel := context.WithTimeout(s.ctx, totalTimeout)
	defer totalCancel()

	client, err := s.getOrCreateWSClient(bot)
	if err != nil {
		s.log.Error("[handleStreamRequest] 获取 WebSocket 客户端失败", zap.Error(err))
		s.sendTextMessage(msg.SenderID, fmt.Sprintf("连接 AI 服务失败: %v", err))
		return err
	}

	contentChan := make(chan string, 100)
	doneChan := make(chan bool, 1)
	errChan := make(chan error, 1)

	sessionKey := BuildSessionID(msg.Channel, msg.ChatType, msg.SenderOpenID)
	go func() {
		err := client.ChatSendStream(totalCtx, &ChatSendParams{
			Message:    content,
			AgentID:    bot.AgentID,
			SessionKey: sessionKey,
		}, func(chunk string) {
			contentChan <- chunk
		})
		if err != nil {
			errChan <- err
		} else {
			doneChan <- true
		}
	}()

	var currentContent, lastSentContent string
	retryCount := 0
	isComplete := false

	ticker := time.NewTicker(chunkTimeout)
	defer ticker.Stop()

	for !isComplete && retryCount <= maxRetries {
		select {
		case <-totalCtx.Done():
			if currentContent != "" && currentContent != lastSentContent {
				s.sendMarkdownMessage(msg.SenderID, currentContent)
			} else if currentContent == "" {
				s.mu.RLock()
				timeoutMsg := s.cfg.TimeoutMessage
				s.mu.RUnlock()
				s.sendTimeoutMessage(msg.SenderID, totalTimeout, timeoutMsg)
			}
			return nil
		case err := <-errChan:
			s.log.Error("[handleStreamRequest] 流式请求错误", zap.Error(err))
			s.sendTextMessage(msg.SenderID, fmt.Sprintf("处理出错: %v", err))
			return err
		case content := <-contentChan:
			if content != "" {
				currentContent += content
			}
		case <-doneChan:
			isComplete = true
		case <-ticker.C:
			if currentContent != "" && currentContent != lastSentContent {
				s.sendMarkdownMessage(msg.SenderID, currentContent)
				lastSentContent = currentContent
				retryCount++
			}
		}
	}

	if currentContent != "" && currentContent != lastSentContent {
		s.sendMarkdownMessage(msg.SenderID, currentContent)
	} else if currentContent == "" {
		s.sendTextMessage(msg.SenderID, "未能获取到响应内容")
	}
	return nil
}

func (s *GoclawService) sendTextMessage(toUserID, content string) error {
	s.mu.RLock()
	sender := s.sender
	s.mu.RUnlock()
	if sender != nil {
		return sender.SendTextMessage(toUserID, content)
	}
	s.log.Warn("[sendTextMessage] sender 未初始化", zap.String("to_user", toUserID), zap.String("content", content))
	return nil
}

func (s *GoclawService) sendMarkdownMessage(toUserID, content string) error {
	s.mu.RLock()
	sender := s.sender
	s.mu.RUnlock()
	if sender != nil {
		return sender.SendMarkdownMessage(toUserID, content)
	}
	s.log.Warn("[sendMarkdownMessage] sender 未初始化", zap.String("to_user", toUserID), zap.String("content", content))
	return nil
}

func (s *GoclawService) sendTimeoutMessage(toUserID string, timeout time.Duration, timeoutMsgTemplate string) {
	if timeoutMsgTemplate == "" {
		timeoutMsgTemplate = s.cfg.TimeoutMessage
	}
	s.sendTextMessage(toUserID, fmt.Sprintf(timeoutMsgTemplate, timeout))
}

func (s *GoclawService) DownloadFile(fileURL, outputPath string) (string, error) {
	resp, err := s.httpClient.Get(fileURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	ext := ".bin"
	switch resp.Header.Get("Content-Type") {
	case "image/jpeg":
		ext = ".jpg"
	case "image/png":
		ext = ".png"
	case "image/gif":
		ext = ".gif"
	case "image/webp":
		ext = ".webp"
	}

	out, err := os.Create(outputPath + ext)
	if err != nil {
		return "", err
	}
	defer out.Close()

	if _, err := io.Copy(out, resp.Body); err != nil {
		return "", err
	}
	return outputPath + ext, nil
}
