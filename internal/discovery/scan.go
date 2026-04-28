package discovery

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ixx/xtx/internal/model"
)

// MaxScanIPs 单次扫描覆盖的 IP 上限。误填一个 /16 会展开成 6.5w 个，会被截断到这个数。
const MaxScanIPs = 8192

const (
	scanConcurrency  = 64
	scanProbeTimeout = 1 * time.Second
	scanInterval     = 5 * time.Minute
	scanStartupDelay = 3 * time.Second
)

// ParseScanTargets 把用户填的多行扫描目标解析成去重后的 IPv4 列表。
// 支持的写法（只允许第 4 段含通配/范围，跨段通配会被拒绝）：
//   - 单 IP：     10.10.1.5
//   - CIDR：      10.10.1.0/24
//   - 末位通配：  10.10.10.*
//   - 末位范围：  10.10.10.5-50
//
// 非法行不会让整体失败，会以 "原文: 原因" 的形式收集进 errs 一并返回，由 UI 提示用户。
func ParseScanTargets(lines []string) (ips []net.IP, errs []string) {
	seen := make(map[string]bool)
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parsed, err := parseOneScanTarget(line)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", line, err))
			continue
		}
		for _, ip := range parsed {
			key := ip.String()
			if seen[key] {
				continue
			}
			seen[key] = true
			ips = append(ips, ip)
		}
	}
	return ips, errs
}

func parseOneScanTarget(line string) ([]net.IP, error) {
	// CIDR
	if strings.Contains(line, "/") {
		_, ipnet, err := net.ParseCIDR(line)
		if err != nil {
			return nil, fmt.Errorf("无效 CIDR: %v", err)
		}
		if ipnet.IP.To4() == nil {
			return nil, fmt.Errorf("仅支持 IPv4")
		}
		return expandCIDR(ipnet), nil
	}

	// 末位通配 a.b.c.*
	if strings.HasSuffix(line, ".*") {
		prefix := strings.TrimSuffix(line, ".*")
		parts := strings.Split(prefix, ".")
		if len(parts) != 3 {
			return nil, fmt.Errorf("通配只允许末位，如 10.10.10.*")
		}
		for _, p := range parts {
			if strings.ContainsAny(p, "*?") {
				return nil, fmt.Errorf("通配只允许末位，如 10.10.10.*")
			}
			n, err := strconv.Atoi(p)
			if err != nil || n < 0 || n > 255 {
				return nil, fmt.Errorf("前三段需为 0-255 的整数")
			}
		}
		base := net.ParseIP(prefix + ".0").To4()
		if base == nil {
			return nil, fmt.Errorf("无效 IP 前缀")
		}
		return expandRange(base, 1, 254), nil
	}

	// 范围 a.b.c.x-y（短横线只允许出现在第 4 段）
	if idx := strings.LastIndex(line, "-"); idx > 0 {
		head, tail := line[:idx], line[idx+1:]
		headIP := net.ParseIP(head).To4()
		if headIP == nil {
			return nil, fmt.Errorf("范围起始不是合法 IPv4")
		}
		end, err := strconv.Atoi(tail)
		if err != nil || end < 0 || end > 255 {
			return nil, fmt.Errorf("范围末位需在 0-255")
		}
		start := int(headIP[3])
		if end < start {
			return nil, fmt.Errorf("范围末位小于起始")
		}
		base := make(net.IP, 4)
		copy(base, headIP)
		base[3] = 0
		return expandRange(base, start, end), nil
	}

	// 单 IP
	if strings.ContainsAny(line, "*?") {
		return nil, fmt.Errorf("通配只允许末位，如 10.10.10.*")
	}
	ip := net.ParseIP(line).To4()
	if ip == nil {
		return nil, fmt.Errorf("不是合法 IPv4")
	}
	return []net.IP{ip}, nil
}

// expandCIDR 把 CIDR 展开为主机 IP 列表，跳过网络/广播地址，并按 MaxScanIPs 截断。
func expandCIDR(ipnet *net.IPNet) []net.IP {
	ones, bits := ipnet.Mask.Size()
	if bits != 32 {
		return nil
	}
	hostBits := bits - ones
	base := ipnet.IP.To4()
	if hostBits == 0 {
		return []net.IP{append(net.IP(nil), base...)}
	}
	total := uint32(1) << uint32(hostBits)
	out := make([]net.IP, 0, total)
	baseV := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
	for i := uint32(0); i < total; i++ {
		v := baseV + i
		ip := net.IP{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
		// /24 及更窄子网跳过 .0 / .255
		if hostBits <= 8 && (ip[3] == 0 || ip[3] == 255) {
			continue
		}
		out = append(out, ip)
		if len(out) >= MaxScanIPs {
			break
		}
	}
	return out
}

func expandRange(base net.IP, start, end int) []net.IP {
	out := make([]net.IP, 0, end-start+1)
	for i := start; i <= end; i++ {
		ip := net.IP{base[0], base[1], base[2], byte(i)}
		out = append(out, ip)
	}
	return out
}

// SetScanTargets 解析并保存扫描目标。返回成功解析出的 IP 数和每行错误。
// IP 总数会被截断到 MaxScanIPs。
func (s *Service) SetScanTargets(lines []string) (int, []string) {
	ips, errs := ParseScanTargets(lines)
	if len(ips) > MaxScanIPs {
		errs = append(errs, fmt.Sprintf("目标过多（%d 个），已截断到 %d 个", len(ips), MaxScanIPs))
		ips = ips[:MaxScanIPs]
	}
	s.mu.Lock()
	s.scanIPs = ips
	s.mu.Unlock()
	return len(ips), errs
}

// ScanNow 立即用当前已保存的扫描目标发起一轮扫描，返回实际发出的探测包数。
func (s *Service) ScanNow() int {
	s.mu.RLock()
	ips := append([]net.IP(nil), s.scanIPs...)
	s.mu.RUnlock()
	return s.probeUnicast(ips)
}

// probeUnicast 向给定 IP 列表发送 UDP 单播 ONLINE 探测包。
// 跨网段 UDP 广播一般会被路由器丢弃，所以发现别网段的客户端必须靠单播探测。
// 目标客户端的 receive 协程会把发送方加入用户表并回 HEARTBEAT，从而双向可见。
func (s *Service) probeUnicast(ips []net.IP) int {
	if len(ips) == 0 {
		return 0
	}
	if len(ips) > MaxScanIPs {
		ips = ips[:MaxScanIPs]
	}
	msg := model.BroadcastMsg{
		Type:     model.MsgOnline,
		Nickname: s.nickname,
		IP:       s.localIP,
		TCPPort:  s.tcpPort,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("scan 序列化失败: %v", err)
		return 0
	}

	skip := s.localIP
	jobs := make(chan net.IP, scanConcurrency)
	var wg sync.WaitGroup
	var sent int64
	for w := 0; w < scanConcurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ip := range jobs {
				if ip.String() == skip {
					continue
				}
				udpAddr := &net.UDPAddr{IP: ip, Port: s.udpPort}
				conn, err := net.DialUDP("udp4", nil, udpAddr)
				if err != nil {
					continue
				}
				_ = conn.SetWriteDeadline(time.Now().Add(scanProbeTimeout))
				if _, err := conn.Write(data); err == nil {
					atomic.AddInt64(&sent, 1)
				}
				conn.Close()
			}
		}()
	}
	for _, ip := range ips {
		jobs <- ip
	}
	close(jobs)
	wg.Wait()
	return int(sent)
}

// scanLoop 启动后延迟 scanStartupDelay 跑首轮，之后每 scanInterval 跑一次。
// 没设置扫描目标时是空操作，开销可以忽略。
func (s *Service) scanLoop() {
	startup := time.NewTimer(scanStartupDelay)
	defer startup.Stop()
	ticker := time.NewTicker(scanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.quit:
			return
		case <-startup.C:
			s.runScheduledScan()
		case <-ticker.C:
			s.runScheduledScan()
		}
	}
}

func (s *Service) runScheduledScan() {
	s.mu.RLock()
	n := len(s.scanIPs)
	ips := append([]net.IP(nil), s.scanIPs...)
	s.mu.RUnlock()
	if n == 0 {
		return
	}
	sent := s.probeUnicast(ips)
	log.Printf("自动扫描完成：目标 %d 个，发出 %d 个探测包", n, sent)
}
