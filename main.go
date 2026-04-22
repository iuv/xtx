package main

import (
	_ "embed"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/ixx/xtx/internal/chat"
	"github.com/ixx/xtx/internal/discovery"
	"github.com/ixx/xtx/internal/model"
	"github.com/ixx/xtx/internal/storage"
	"github.com/ixx/xtx/internal/ui"
)

//go:embed logo.jpeg
var logoData []byte

func main() {
	// CLI 参数：支持多实例运行以便单机测试
	tcpPort := flag.Int("tcp", model.DefaultTCPPort, "TCP 聊天端口")
	udpPort := flag.Int("udp", model.DefaultUDPPort, "UDP 发现端口")
	nick := flag.String("nick", "", "昵称（覆盖存储设置）")
	dataPath := flag.String("data", "", "数据目录（默认 ~/.xtx）")
	flag.Parse()

	// 数据目录
	var dataDir string
	if *dataPath != "" {
		dataDir = *dataPath
	} else {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			log.Fatal("获取用户目录失败:", err)
		}
		dataDir = filepath.Join(homeDir, ".xtx")
	}

	// 初始化存储
	store, err := storage.New(dataDir)
	if err != nil {
		log.Fatal("初始化存储失败:", err)
	}

	// 获取昵称
	nickname := *nick
	if nickname == "" {
		nickname, _ = store.GetSetting("nickname")
	}
	if nickname == "" {
		hostname, _ := os.Hostname()
		if hostname == "" {
			hostname = "XTX用户"
		}
		nickname = hostname
	}

	// 启动发现服务
	disc := discovery.NewService(nickname, *tcpPort, *udpPort)
	if err := disc.Start(); err != nil {
		log.Fatal("启动发现服务失败:", err)
	}
	// 加载额外广播地址
	extraBroadcast, _ := store.GetSetting("extra_broadcast")
	if extraBroadcast != "" {
		var addrs []string
		for _, line := range strings.Split(extraBroadcast, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				addrs = append(addrs, line)
			}
		}
		if len(addrs) > 0 {
			disc.SetExtraBroadcastAddrs(addrs)
		}
	}

	fmt.Printf("XTX 启动成功 - 昵称: %s, IP: %s, TCP: %d, UDP: %d\n",
		nickname, disc.LocalIP(), *tcpPort, *udpPort)

	// 启动聊天服务
	chatSvc := chat.NewService(*tcpPort)
	if err := chatSvc.Start(); err != nil {
		disc.Stop()
		log.Fatal("启动聊天服务失败:", err)
	}

	// 启动GUI
	application := ui.New(disc, chatSvc, store)
	application.SetIcon(logoData)
	application.Run()
}
