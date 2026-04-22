package chat

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/ixx/xtx/internal/model"
)

// FileEvent 文件传输事件
type FileEvent struct {
	Msg *model.ChatMessage
}

// Service 聊天服务
type Service struct {
	tcpPort    int
	listener   net.Listener
	incoming   chan *model.ChatMessage
	fileEvents chan *FileEvent
	quit       chan struct{}
	mu         sync.Mutex
}

// NewService 创建聊天服务
func NewService(tcpPort int) *Service {
	return &Service{
		tcpPort:    tcpPort,
		incoming:   make(chan *model.ChatMessage, 100),
		fileEvents: make(chan *FileEvent, 100),
		quit:       make(chan struct{}),
	}
}

// Incoming 返回收到的消息通道
func (s *Service) Incoming() <-chan *model.ChatMessage {
	return s.incoming
}

// FileEvents 返回文件传输事件通道
func (s *Service) FileEvents() <-chan *FileEvent {
	return s.fileEvents
}

// Start 启动TCP监听
func (s *Service) Start() error {
	addr := fmt.Sprintf(":%d", s.tcpPort)
	listener, err := net.Listen("tcp4", addr)
	if err != nil {
		return fmt.Errorf("监听TCP端口 %d 失败: %w", s.tcpPort, err)
	}
	s.listener = listener

	go s.accept()
	return nil
}

// Stop 停止聊天服务
func (s *Service) Stop() {
	close(s.quit)
	if s.listener != nil {
		s.listener.Close()
	}
}

func (s *Service) accept() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.quit:
				return
			default:
				log.Printf("接受TCP连接失败: %v", err)
				continue
			}
		}
		go s.handleConn(conn)
	}
}

func (s *Service) handleConn(conn net.Conn) {
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))

	data, err := io.ReadAll(conn)
	if err != nil {
		log.Printf("读取TCP消息失败: %v", err)
		return
	}

	var msg model.ChatMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("解析TCP消息失败: %v", err)
		return
	}

	// 文件传输相关消息发送到 fileEvents 通道
	switch msg.Type {
	case model.ChatFileRequest, model.ChatFileAccept, model.ChatFileReject,
		model.ChatFileData, model.ChatFileComplete:
		select {
		case s.fileEvents <- &FileEvent{Msg: &msg}:
		case <-s.quit:
		}
	default:
		select {
		case s.incoming <- &msg:
		case <-s.quit:
		}
	}
}

// SendMessage 发送消息给指定IP:Port
func (s *Service) SendMessage(ip string, port int, msg *model.ChatMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("序列化消息失败: %w", err)
	}

	addr := fmt.Sprintf("%s:%d", ip, port)
	conn, err := net.DialTimeout("tcp4", addr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("连接 %s 失败: %w", addr, err)
	}
	defer conn.Close()

	conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, err = conn.Write(data)
	if err != nil {
		return fmt.Errorf("发送消息失败: %w", err)
	}
	return nil
}

// SendToGroup 群发消息给多个成员
func (s *Service) SendToGroup(members []model.User, msg *model.ChatMessage, localKey string) map[string]error {
	errors := make(map[string]error)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, u := range members {
		if u.Key() == localKey || !u.Online {
			continue
		}
		wg.Add(1)
		go func(user model.User) {
			defer wg.Done()
			if err := s.SendMessage(user.IP, user.TCPPort, msg); err != nil {
				mu.Lock()
				errors[user.IP] = err
				mu.Unlock()
			}
		}(u)
	}
	wg.Wait()
	return errors
}
