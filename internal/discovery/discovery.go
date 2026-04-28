package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/ixx/xtx/internal/model"
)

// EventType 事件类型
type EventType int

const (
	EventUserOnline EventType = iota
	EventUserOffline
	EventGroupCreated
	EventGroupUpdated
	EventGroupQuit
)

// Event 发现事件
type Event struct {
	Type  EventType
	User  *model.User
	Group *model.Group
	IP    string // 事件来源IP
}

// Service 用户发现服务
type Service struct {
	nickname string
	tcpPort  int
	udpPort  int
	localIP  string

	conn            *net.UDPConn
	users           map[string]*model.User  // key: IP:TCPPort
	groups          map[string]*model.Group  // key: GroupID
	extraBroadcast  []string                 // 额外广播地址
	scanIPs         []net.IP                 // 跨网段单播扫描目标（已展开）
	mu              sync.RWMutex

	events chan Event
	quit   chan struct{}
}

// NewService 创建发现服务
func NewService(nickname string, tcpPort, udpPort int) *Service {
	return &Service{
		nickname: nickname,
		tcpPort:  tcpPort,
		udpPort:  udpPort,
		users:    make(map[string]*model.User),
		groups:   make(map[string]*model.Group),
		events:   make(chan Event, 100),
		quit:     make(chan struct{}),
	}
}

// Events 返回事件通道
func (s *Service) Events() <-chan Event {
	return s.events
}

// emit 非阻塞发送事件。通道满则丢弃：UI 下一次事件或定时刷新可恢复状态，
// 不能让消费者慢导致 receive/checkTimeout 卡住。
func (s *Service) emit(evt Event) {
	select {
	case s.events <- evt:
	default:
		log.Printf("发现事件通道已满，丢弃事件 type=%d", evt.Type)
	}
}

// GetUsers 获取所有用户
func (s *Service) GetUsers() []*model.User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	users := make([]*model.User, 0, len(s.users))
	for _, u := range s.users {
		users = append(users, u)
	}
	return users
}

// GetGroups 获取所有群聊
func (s *Service) GetGroups() []*model.Group {
	s.mu.RLock()
	defer s.mu.RUnlock()
	groups := make([]*model.Group, 0, len(s.groups))
	for _, g := range s.groups {
		groups = append(groups, g)
	}
	return groups
}

// GetGroup 获取指定群聊
func (s *Service) GetGroup(groupID string) *model.Group {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.groups[groupID]
}

// AddGroup 添加群聊（本地创建时使用）
func (s *Service) AddGroup(g *model.Group) {
	s.mu.Lock()
	s.groups[g.ID] = g
	s.mu.Unlock()
}

// RemoveGroup 移除群聊
func (s *Service) RemoveGroup(groupID string) {
	s.mu.Lock()
	delete(s.groups, groupID)
	s.mu.Unlock()
}

// LocalIP 返回本地IP
func (s *Service) LocalIP() string {
	return s.localIP
}

// Nickname 返回昵称
func (s *Service) Nickname() string {
	return s.nickname
}

// SetNickname 设置昵称并立即发送心跳广播
func (s *Service) SetNickname(name string) {
	s.nickname = name
	s.broadcast(model.MsgHeartbeat)
}

// SetExtraBroadcastAddrs 设置额外广播地址
func (s *Service) SetExtraBroadcastAddrs(addrs []string) {
	s.mu.Lock()
	s.extraBroadcast = addrs
	s.mu.Unlock()
}

// Start 启动发现服务
func (s *Service) Start() error {
	ip, err := getLocalIP()
	if err != nil {
		return fmt.Errorf("获取本地IP失败: %w", err)
	}
	s.localIP = ip

	// 使用 SO_REUSEADDR + SO_REUSEPORT 允许同一端口多实例（便于单机测试）
	lc := net.ListenConfig{
		Control: reuseAddrControl,
	}
	pc, err := lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf(":%d", s.udpPort))
	if err != nil {
		return fmt.Errorf("监听UDP端口 %d 失败: %w", s.udpPort, err)
	}
	s.conn = pc.(*net.UDPConn)

	// 发送上线广播
	s.broadcast(model.MsgOnline)

	// 启动接收协程
	go s.receive()
	// 启动心跳协程
	go s.heartbeat()
	// 启动超时检测协程
	go s.checkTimeout()
	// 启动跨网段扫描协程（无目标时空转，开销可忽略）
	go s.scanLoop()

	return nil
}

// Probe 主动发送上线广播探测其他用户
func (s *Service) Probe() {
	s.broadcast(model.MsgOnline)
}

// Stop 停止发现服务
func (s *Service) Stop() {
	s.broadcast(model.MsgOffline)
	close(s.quit)
	if s.conn != nil {
		s.conn.Close()
	}
}

// BroadcastGroupCreate 广播创建群聊
func (s *Service) BroadcastGroupCreate(g *model.Group) {
	msg := model.BroadcastMsg{
		Type:      model.MsgGroupCreate,
		Nickname:  s.nickname,
		IP:        s.localIP,
		TCPPort:   s.tcpPort,
		GroupID:   g.ID,
		GroupName: g.Name,
		Members:   g.Members,
	}
	s.broadcastMsg(msg)
}

// BroadcastGroupUpdate 广播更新群聊
func (s *Service) BroadcastGroupUpdate(g *model.Group) {
	msg := model.BroadcastMsg{
		Type:      model.MsgGroupUpdate,
		Nickname:  s.nickname,
		IP:        s.localIP,
		TCPPort:   s.tcpPort,
		GroupID:   g.ID,
		GroupName: g.Name,
		Members:   g.Members,
	}
	s.broadcastMsg(msg)
}

// BroadcastGroupQuit 广播退出群聊
func (s *Service) BroadcastGroupQuit(groupID string) {
	msg := model.BroadcastMsg{
		Type:     model.MsgGroupQuit,
		Nickname: s.nickname,
		IP:       s.localIP,
		TCPPort:  s.tcpPort,
		GroupID:  groupID,
	}
	s.broadcastMsg(msg)
}

func (s *Service) broadcast(msgType string) {
	msg := model.BroadcastMsg{
		Type:     msgType,
		Nickname: s.nickname,
		IP:       s.localIP,
		TCPPort:  s.tcpPort,
	}
	s.broadcastMsg(msg)
}

func (s *Service) broadcastMsg(msg model.BroadcastMsg) {
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("序列化广播消息失败: %v", err)
		return
	}

	// 收集所有广播地址：子网广播地址 + 255.255.255.255
	addrs := getSubnetBroadcastAddrs()
	addrs = append(addrs, net.IPv4bcast.String())

	// 加上额外广播地址
	s.mu.RLock()
	addrs = append(addrs, s.extraBroadcast...)
	s.mu.RUnlock()

	// 去重
	seen := make(map[string]bool)
	for _, addr := range addrs {
		if seen[addr] {
			continue
		}
		seen[addr] = true
		udpAddr := &net.UDPAddr{
			IP:   net.ParseIP(addr),
			Port: s.udpPort,
		}
		conn, err := net.DialUDP("udp4", nil, udpAddr)
		if err != nil {
			log.Printf("发送广播到 %s 失败: %v", addr, err)
			continue
		}
		conn.Write(data)
		conn.Close()
	}
}

func (s *Service) receive() {
	buf := make([]byte, 4096)
	for {
		select {
		case <-s.quit:
			return
		default:
		}

		s.conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, _, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			select {
			case <-s.quit:
				return
			default:
				log.Printf("接收UDP消息失败: %v", err)
				continue
			}
		}

		var msg model.BroadcastMsg
		if err := json.Unmarshal(buf[:n], &msg); err != nil {
			continue
		}

		// 忽略自己的消息（同IP+同TCP端口才是自己，支持同机多实例测试）
		if msg.IP == s.localIP && msg.TCPPort == s.tcpPort {
			continue
		}

		s.handleMessage(msg)
	}
}

func (s *Service) handleMessage(msg model.BroadcastMsg) {
	switch msg.Type {
	case model.MsgOnline, model.MsgHeartbeat:
		key := model.UserKey(msg.IP, msg.TCPPort)
		s.mu.Lock()
		user, exists := s.users[key]
		if !exists {
			user = &model.User{
				Nickname: msg.Nickname,
				IP:       msg.IP,
				TCPPort:  msg.TCPPort,
				Online:   true,
			}
			s.users[key] = user
		}
		user.Nickname = msg.Nickname
		user.LastSeen = time.Now()
		user.Online = true
		s.mu.Unlock()

		if !exists || msg.Type == model.MsgOnline {
			s.emit(Event{Type: EventUserOnline, User: user})
			// 回复自己的在线状态
			if msg.Type == model.MsgOnline {
				s.broadcast(model.MsgHeartbeat)
			}
		}

	case model.MsgOffline:
		key := model.UserKey(msg.IP, msg.TCPPort)
		s.mu.Lock()
		if user, ok := s.users[key]; ok {
			user.Online = false
			s.mu.Unlock()
			s.emit(Event{Type: EventUserOffline, User: user})
		} else {
			s.mu.Unlock()
		}

	case model.MsgGroupCreate:
		g := &model.Group{
			ID:        msg.GroupID,
			Name:      msg.GroupName,
			Members:   msg.Members,
			CreatorIP: msg.IP,
		}
		// 只有自己是成员才加入
		if s.isMember(g.Members) {
			s.mu.Lock()
			s.groups[g.ID] = g
			s.mu.Unlock()
			s.emit(Event{Type: EventGroupCreated, Group: g, IP: msg.IP})
		}

	case model.MsgGroupUpdate:
		s.mu.Lock()
		if g, ok := s.groups[msg.GroupID]; ok {
			g.Name = msg.GroupName
			g.Members = msg.Members
			s.mu.Unlock()
			s.emit(Event{Type: EventGroupUpdated, Group: g, IP: msg.IP})
		} else if s.isMember(msg.Members) {
			// 新加入的群
			g := &model.Group{
				ID:        msg.GroupID,
				Name:      msg.GroupName,
				Members:   msg.Members,
				CreatorIP: msg.IP,
			}
			s.groups[g.ID] = g
			s.mu.Unlock()
			s.emit(Event{Type: EventGroupCreated, Group: g, IP: msg.IP})
		} else {
			s.mu.Unlock()
		}

	case model.MsgGroupQuit:
		s.mu.Lock()
		if g, ok := s.groups[msg.GroupID]; ok {
			// 从成员列表移除
			members := make([]string, 0, len(g.Members))
			for _, m := range g.Members {
				if m != msg.IP {
					members = append(members, m)
				}
			}
			g.Members = members
			s.mu.Unlock()
			s.emit(Event{Type: EventGroupQuit, Group: g, IP: msg.IP})
		} else {
			s.mu.Unlock()
		}

	case model.MsgGroupSync:
		// 收到同步请求，广播自己知道的群
		s.mu.RLock()
		groups := make([]*model.Group, 0)
		for _, g := range s.groups {
			if s.containsMember(g.Members, msg.IP) {
				groups = append(groups, g)
			}
		}
		s.mu.RUnlock()
		for _, g := range groups {
			s.BroadcastGroupCreate(g)
		}
	}
}

func (s *Service) isMember(members []string) bool {
	for _, m := range members {
		if m == s.localIP {
			return true
		}
	}
	return false
}

func (s *Service) containsMember(members []string, ip string) bool {
	for _, m := range members {
		if m == ip {
			return true
		}
	}
	return false
}

func (s *Service) heartbeat() {
	ticker := time.NewTicker(model.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.quit:
			return
		case <-ticker.C:
			s.broadcast(model.MsgHeartbeat)
		}
	}
}

func (s *Service) checkTimeout() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.quit:
			return
		case <-ticker.C:
			var offlined []*model.User
			s.mu.Lock()
			for _, user := range s.users {
				if user.Online && time.Since(user.LastSeen) > model.HeartbeatTimeout {
					user.Online = false
					offlined = append(offlined, user)
				}
			}
			s.mu.Unlock()
			for _, u := range offlined {
				s.emit(Event{Type: EventUserOffline, User: u})
			}
		}
	}
}

// GetUserByKey 根据用户标识（IP:Port）获取用户
func (s *Service) GetUserByKey(key string) *model.User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.users[key]
}

// GetUserByIP 根据IP获取用户（向后兼容，匹配第一个该IP的用户）
func (s *Service) GetUserByIP(ip string) *model.User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, u := range s.users {
		if u.IP == ip {
			return u
		}
	}
	return nil
}

// TCPPort 返回本机TCP端口
func (s *Service) TCPPort() int {
	return s.tcpPort
}

func getLocalIP() (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}
	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok && !ipNet.IP.IsLoopback() {
			if ipNet.IP.To4() != nil {
				return ipNet.IP.String(), nil
			}
		}
	}
	return "", fmt.Errorf("未找到局域网IP地址")
}

// getSubnetBroadcastAddrs 获取所有网络接口的子网广播地址
func getSubnetBroadcastAddrs() []string {
	var result []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return result
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP.To4() == nil {
				continue
			}
			// 计算广播地址: IP | ^Mask
			ip := ipNet.IP.To4()
			mask := ipNet.Mask
			bcast := make(net.IP, 4)
			for i := 0; i < 4; i++ {
				bcast[i] = ip[i] | ^mask[i]
			}
			result = append(result, bcast.String())
		}
	}
	return result
}
