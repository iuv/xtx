package model

import (
	"fmt"
	"time"
)

// UDP 广播消息类型
const (
	MsgOnline      = "ONLINE"
	MsgHeartbeat   = "HEARTBEAT"
	MsgOffline     = "OFFLINE"
	MsgGroupCreate = "GROUP_CREATE"
	MsgGroupUpdate = "GROUP_UPDATE"
	MsgGroupQuit   = "GROUP_QUIT"
	MsgGroupSync   = "GROUP_SYNC"
)

// TCP 消息类型
const (
	ChatText  = "text"
	ChatImage = "image"
	ChatFile  = "file" // 文件传输完成后在聊天记录中显示

	// 文件传输控制消息
	ChatFileRequest  = "file_request"  // 请求发送文件
	ChatFileAccept   = "file_accept"   // 接受文件
	ChatFileReject   = "file_reject"   // 拒绝文件
	ChatFileData     = "file_data"     // 文件数据块
	ChatFileComplete = "file_complete" // 传输完成
)

// TCP 消息范围
const (
	ScopePrivate = "private"
	ScopeGroup   = "group"
)

// 默认端口
const (
	DefaultUDPPort = 9527
	DefaultTCPPort = 9528
)

// 心跳间隔和超时
const (
	HeartbeatInterval = 30 * time.Second
	HeartbeatTimeout  = 90 * time.Second
)

// User 在线用户
type User struct {
	Nickname string `json:"nickname"`
	IP       string `json:"ip"`
	TCPPort  int    `json:"tcp_port"`
	LastSeen time.Time
	Online   bool
}

// Key 返回用户唯一标识
func (u *User) Key() string {
	return UserKey(u.IP, u.TCPPort)
}

// BroadcastMsg UDP广播消息
type BroadcastMsg struct {
	Type     string   `json:"type"`
	Nickname string   `json:"nickname"`
	IP       string   `json:"ip"`
	TCPPort  int      `json:"tcp_port"`
	GroupID  string   `json:"group_id,omitempty"`
	GroupName string  `json:"group_name,omitempty"`
	Members  []string `json:"members,omitempty"` // 成员IP列表
}

// UserKey 返回用户唯一标识（IP:TCPPort），支持同机多实例
func UserKey(ip string, port int) string {
	return fmt.Sprintf("%s:%d", ip, port)
}

// ChatMessage TCP聊天消息
type ChatMessage struct {
	Type      string `json:"type"`      // text|image|file_request|file_accept|file_reject|file_data|file_complete
	Scope     string `json:"scope"`     // private|group
	GroupID   string `json:"group_id,omitempty"`
	From      string `json:"from"`
	FromIP    string `json:"from_ip"`
	FromPort  int    `json:"from_port,omitempty"` // 发送方TCP端口，支持同机多实例
	Timestamp int64  `json:"timestamp"`
	Content   string `json:"content"`
	Filename  string `json:"filename,omitempty"`

	// 文件传输字段
	FileID     string `json:"file_id,omitempty"`     // 文件传输唯一ID
	FileSize   int64  `json:"file_size,omitempty"`   // 文件大小(字节)
	ChunkIdx   int    `json:"chunk_idx,omitempty"`   // 分块索引
	ChunkTotal int    `json:"chunk_total,omitempty"` // 总块数
}

// Group 群聊
type Group struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Members   []string `json:"members"` // 成员IP列表
	CreatorIP string   `json:"creator_ip"`
}

// StoredMessage 存储的聊天记录
type StoredMessage struct {
	ID        int64
	SessionID string // 单聊为对方IP, 群聊为群ID
	Scope     string
	FromNick  string
	FromIP    string
	Type      string
	Content   string
	Filename  string
	Timestamp int64
}
