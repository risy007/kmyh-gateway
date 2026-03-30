package goclaw

import (
	"encoding/json"
	"strings"
	"time"
)

// ============ 统一渠道消息结构 ============

// ChannelType 渠道类型枚举
type ChannelType string

const (
	ChannelFeishu   ChannelType = "feishu"
	ChannelWeixin   ChannelType = "weixin"
	ChannelQQ       ChannelType = "qq"
	ChannelWechat   ChannelType = "wechat"
	ChannelTelegram ChannelType = "telegram"
	ChannelDiscord  ChannelType = "discord"
)

// MessageType 消息类型枚举
type MessageType string

const (
	MsgTypeText  MessageType = "text"
	MsgTypeImage MessageType = "image"
	MsgTypeFile  MessageType = "file"
	MsgTypeVideo MessageType = "video"
	MsgTypeVoice MessageType = "voice"
	MsgTypeMixed MessageType = "mixed"
)

// ChannelMessage 统一渠道消息结构（解签解密后的明文消息）
type ChannelMessage struct {
	// 消息基本信息
	MsgID   string      `json:"msg_id"`   // 原始消息ID
	MsgType MessageType `json:"msg_type"` // 消息类型
	Channel ChannelType `json:"channel"`  // 渠道类型

	// 发送者信息
	SenderID     string `json:"sender_id"`      // 发送者ID
	SenderName   string `json:"sender_name"`    // 发送者昵称（可选）
	SenderOpenID string `json:"sender_open_id"` // 发送者平台ID

	// 会话信息
	SessionID string `json:"session_id"` // 会话ID，格式: {channel}:{chat_type}:{openid}
	ChatID    string `json:"chat_id"`    // 群/会话ID
	ChatType  string `json:"chat_type"`  // p2p(私聊) / group(群聊)
	IsGroup   bool   `json:"is_group"`   // 是否是群聊

	// 消息内容
	Text       string                 `json:"text,omitempty"`        // 纯文本内容
	MediaFiles []MediaFile            `json:"media_files,omitempty"` // 附件文件列表
	RawContent map[string]interface{} `json:"raw_content,omitempty"` // 原始消息内容

	// 元信息
	Timestamp time.Time `json:"timestamp"`   // 消息时间
	BotUserID string    `json:"bot_user_id"` // 机器人用户ID（用于多bot配置）
}

// MediaFile 媒体文件
type MediaFile struct {
	URL        string `json:"url,omitempty"`         // 文件URL（可选）
	Path       string `json:"path,omitempty"`        // 本地文件路径（可选）
	Filename   string `json:"filename"`              // 文件名
	MimeType   string `json:"mime_type"`             // MIME类型
	Size       int64  `json:"size,omitempty"`        // 文件大小
	Base64Data string `json:"base64_data,omitempty"` // Base64编码数据
}

// ============ GoClaw 消息构建 ============

// GoclawMessage GoClaw 聊天消息结构（用于 chat.send 的 message 字段）
type GoclawMessage struct {
	Message interface{} `json:"message"` // 文本或内容块数组
}

// ContentBlock 内容块（用于多模态消息）
type ContentBlock struct {
	Type     string         `json:"type"` // "text" | "image_url" | "file"
	Text     string         `json:"text,omitempty"`
	ImageURL *ImageURLBlock `json:"image_url,omitempty"`
	File     *FileBlock     `json:"file,omitempty"`
}

// ImageURLBlock 图片内容块
type ImageURLBlock struct {
	URL    string `json:"url"`    // 支持 data: URI
	Detail string `json:"detail"` // "low" | "high" | "auto"
}

// FileBlock 文件内容块
type FileBlock struct {
	Filename string `json:"filename"`  // 文件名
	FileData string `json:"file_data"` // Base64编码的文件数据 (data: mime/type;base64,xxx)
}

// ============ 构建函数 ============

// BuildTextMessage 构建纯文本消息
func BuildTextMessage(text string) *GoclawMessage {
	return &GoclawMessage{
		Message: text,
	}
}

// BuildMixedMessage 构建多模态消息
func BuildMixedMessage(text string, files []MediaFile) *GoclawMessage {
	var blocks []ContentBlock

	// 添加文本块
	if text != "" {
		blocks = append(blocks, ContentBlock{
			Type: "text",
			Text: text,
		})
	}

	// 添加媒体文件块
	for _, file := range files {
		block := buildMediaBlock(file)
		if block != nil {
			blocks = append(blocks, *block)
		}
	}

	if len(blocks) == 0 {
		return BuildTextMessage("")
	}

	if len(blocks) == 1 && blocks[0].Type == "text" {
		return BuildTextMessage(blocks[0].Text)
	}

	return &GoclawMessage{
		Message: blocks,
	}
}

// buildMediaBlock 构建媒体文件块
func buildMediaBlock(file MediaFile) *ContentBlock {
	ext := getExtFromMime(file.MimeType)
	isImage := isImageMime(file.MimeType)

	// 如果没有 MIME 类型，根据扩展名判断
	if file.MimeType == "" {
		isImage = isImageExt(ext)
	}

	if isImage {
		// 构建图片 URL
		imageURL := file.URL
		if file.Base64Data != "" {
			mimeType := file.MimeType
			if mimeType == "" {
				mimeType = getMimeFromExt(ext)
			}
			imageURL = "data:" + mimeType + ";base64," + file.Base64Data
		}

		return &ContentBlock{
			Type: "image_url",
			ImageURL: &ImageURLBlock{
				URL:    imageURL,
				Detail: "auto",
			},
		}
	}

	// 文件块
	if file.Base64Data == "" && file.URL == "" {
		return nil
	}

	fileData := file.Base64Data
	if fileData == "" && file.URL != "" {
		// 如果只有 URL，需要外部下载后转换
		return &ContentBlock{
			Type: "file",
			File: &FileBlock{
				Filename: file.Filename,
				FileData: file.URL, // 暂时用 URL
			},
		}
	}

	mimeType := file.MimeType
	if mimeType == "" {
		mimeType = getMimeFromExt(ext)
	}

	return &ContentBlock{
		Type: "file",
		File: &FileBlock{
			Filename: file.Filename,
			FileData: "data:" + mimeType + ";base64," + file.Base64Data,
		},
	}
}

// ============ 渠道消息转换函数 ============

// BuildSessionID 构建会话ID
// 格式: {channel}:{chat_type}:{openid}
func BuildSessionID(channel ChannelType, chatType, openID string) string {
	return string(channel) + ":" + chatType + ":" + openID
}

// ParseSessionID 解析会话ID
func ParseSessionID(sessionID string) (channel ChannelType, chatType, openID string) {
	parts := strings.SplitN(sessionID, ":", 3)
	if len(parts) >= 3 {
		channel = ChannelType(parts[0])
		chatType = parts[1]
		openID = parts[2]
	}
	return
}

// ToGoclawMessage 将渠道消息转换为 GoClaw 消息格式
func (m *ChannelMessage) ToGoclawMessage() *GoclawMessage {
	// 处理文本内容
	text := m.Text

	// 如果文本为空，尝试从 raw_content 获取
	if text == "" && m.RawContent != nil {
		if content, ok := m.RawContent["content"].(string); ok {
			text = content
		} else if contentBytes, err := json.Marshal(m.RawContent); err == nil {
			text = string(contentBytes)
		}
	}

	// 如果有媒体文件
	if len(m.MediaFiles) > 0 {
		return BuildMixedMessage(text, m.MediaFiles)
	}

	// 纯文本消息
	return BuildTextMessage(text)
}

// ToChatSendParams 转换为 ChatSendParams
func (m *ChannelMessage) ToChatSendParams(agentID string) *ChatSendParams {
	sessionKey := m.SessionID
	if sessionKey == "" {
		sessionKey = BuildSessionID(m.Channel, m.ChatType, m.SenderOpenID)
	}

	return &ChatSendParams{
		Message:    m.ToGoclawMessage().Message,
		AgentID:    agentID,
		SessionKey: sessionKey,
	}
}

// ============ 辅助函数 ============

func isImageMime(mime string) bool {
	return strings.HasPrefix(mime, "image/")
}

func isImageExt(ext string) bool {
	ext = strings.ToLower(ext)
	imageExts := []string{".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp", ".svg"}
	for _, e := range imageExts {
		if ext == e {
			return true
		}
	}
	return false
}

func getExtFromMime(mime string) string {
	switch mime {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/bmp":
		return ".bmp"
	case "image/svg+xml":
		return ".svg"
	case "audio/mpeg":
		return ".mp3"
	case "audio/wav":
		return ".wav"
	case "audio/amr":
		return ".amr"
	case "video/mp4":
		return ".mp4"
	case "video/3gpp":
		return ".3gp"
	case "application/pdf":
		return ".pdf"
	case "text/plain":
		return ".txt"
	default:
		return ""
	}
}

func getMimeFromExt(ext string) string {
	switch ext {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	case ".svg":
		return "image/svg+xml"
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	case ".amr":
		return "audio/amr"
	case ".mp4":
		return "video/mp4"
	case ".3gp":
		return "video/3gpp"
	case ".pdf":
		return "application/pdf"
	case ".txt":
		return "text/plain"
	default:
		return "application/octet-stream"
	}
}

// MessageTypeFromString 从字符串转换消息类型
func MessageTypeFromString(s string) MessageType {
	switch strings.ToLower(s) {
	case "text":
		return MsgTypeText
	case "image":
		return MsgTypeImage
	case "file":
		return MsgTypeFile
	case "video":
		return MsgTypeVideo
	case "voice", "audio":
		return MsgTypeVoice
	default:
		return MsgTypeText
	}
}

// ChannelTypeFromString 从字符串转换渠道类型
func ChannelTypeFromString(s string) ChannelType {
	switch strings.ToLower(s) {
	case "feishu", "lark":
		return ChannelFeishu
	case "weixin", "wx", "workwx", "wecom":
		return ChannelWeixin
	case "qq":
		return ChannelQQ
	case "wechat":
		return ChannelWechat
	case "telegram", "tg":
		return ChannelTelegram
	case "discord":
		return ChannelDiscord
	default:
		return ChannelType(s)
	}
}
