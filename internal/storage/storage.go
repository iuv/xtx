package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/ixx/xtx/internal/model"
)

// DB 本地存储
type DB struct {
	db      *sql.DB
	dataDir string
}

// New 创建存储实例
func New(dataDir string) (*DB, error) {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}
	imgDir := filepath.Join(dataDir, "images")
	if err := os.MkdirAll(imgDir, 0755); err != nil {
		return nil, fmt.Errorf("创建图片目录失败: %w", err)
	}
	fileDir := filepath.Join(dataDir, "files")
	if err := os.MkdirAll(fileDir, 0755); err != nil {
		return nil, fmt.Errorf("创建文件目录失败: %w", err)
	}

	dbPath := filepath.Join(dataDir, "xtx.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	s := &DB{db: db, dataDir: dataDir}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *DB) init() error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			scope TEXT NOT NULL,
			from_nick TEXT NOT NULL,
			from_ip TEXT NOT NULL,
			type TEXT NOT NULL,
			content TEXT NOT NULL,
			filename TEXT,
			timestamp INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id, timestamp)`,
		`CREATE TABLE IF NOT EXISTS groups (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			creator_ip TEXT NOT NULL,
			members TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
	}
	for _, q := range queries {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("初始化数据库失败: %w", err)
		}
	}
	return nil
}

// Close 关闭数据库
func (s *DB) Close() error {
	return s.db.Close()
}

// ImageDir 返回图片存储目录
func (s *DB) ImageDir() string {
	return filepath.Join(s.dataDir, "images")
}

// FileDir 返回文件存储目录
func (s *DB) FileDir() string {
	return filepath.Join(s.dataDir, "files")
}

// SaveMessage 保存聊天记录
func (s *DB) SaveMessage(msg *model.StoredMessage) error {
	_, err := s.db.Exec(
		`INSERT INTO messages (session_id, scope, from_nick, from_ip, type, content, filename, timestamp)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.SessionID, msg.Scope, msg.FromNick, msg.FromIP, msg.Type, msg.Content, msg.Filename, msg.Timestamp,
	)
	return err
}

// LoadMessages 加载指定会话的聊天记录
func (s *DB) LoadMessages(sessionID string, limit int) ([]*model.StoredMessage, error) {
	rows, err := s.db.Query(
		`SELECT id, session_id, scope, from_nick, from_ip, type, content, filename, timestamp
		 FROM messages WHERE session_id = ? ORDER BY timestamp DESC LIMIT ?`,
		sessionID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []*model.StoredMessage
	for rows.Next() {
		m := &model.StoredMessage{}
		err := rows.Scan(&m.ID, &m.SessionID, &m.Scope, &m.FromNick, &m.FromIP, &m.Type, &m.Content, &m.Filename, &m.Timestamp)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	// 反转为时间正序
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}

// SaveGroup 保存群聊信息
func (s *DB) SaveGroup(g *model.Group) error {
	members := marshalMembers(g.Members)
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO groups (id, name, creator_ip, members) VALUES (?, ?, ?, ?)`,
		g.ID, g.Name, g.CreatorIP, members,
	)
	return err
}

// DeleteGroup 删除群聊
func (s *DB) DeleteGroup(groupID string) error {
	_, err := s.db.Exec(`DELETE FROM groups WHERE id = ?`, groupID)
	return err
}

// LoadGroups 加载所有群聊
func (s *DB) LoadGroups() ([]*model.Group, error) {
	rows, err := s.db.Query(`SELECT id, name, creator_ip, members FROM groups`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []*model.Group
	for rows.Next() {
		g := &model.Group{}
		var members string
		if err := rows.Scan(&g.ID, &g.Name, &g.CreatorIP, &members); err != nil {
			return nil, err
		}
		g.Members = unmarshalMembers(members)
		groups = append(groups, g)
	}
	return groups, nil
}

// GetSetting 获取设置
func (s *DB) GetSetting(key string) (string, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

// SetSetting 保存设置
func (s *DB) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)`, key, value)
	return err
}

func marshalMembers(members []string) string {
	if members == nil {
		members = []string{}
	}
	b, err := json.Marshal(members)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// SearchMessages 搜索聊天记录
func (s *DB) SearchMessages(keyword string, limit int) ([]*model.StoredMessage, error) {
	rows, err := s.db.Query(
		`SELECT id, session_id, scope, from_nick, from_ip, type, content, filename, timestamp
		 FROM messages WHERE type = 'text' AND content LIKE ? ORDER BY timestamp DESC LIMIT ?`,
		"%"+keyword+"%", limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []*model.StoredMessage
	for rows.Next() {
		m := &model.StoredMessage{}
		err := rows.Scan(&m.ID, &m.SessionID, &m.Scope, &m.FromNick, &m.FromIP, &m.Type, &m.Content, &m.Filename, &m.Timestamp)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, nil
}

func unmarshalMembers(s string) []string {
	if s == "" {
		return nil
	}
	// 新格式: JSON 数组
	if strings.HasPrefix(strings.TrimSpace(s), "[") {
		var members []string
		if err := json.Unmarshal([]byte(s), &members); err == nil {
			return members
		}
	}
	// 兼容旧格式: 逗号分隔
	parts := strings.Split(s, ",")
	members := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			members = append(members, p)
		}
	}
	return members
}
