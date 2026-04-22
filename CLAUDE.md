# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目简介

XTX 是一个无服务端的局域网 P2P 聊天工具（类似飞秋），Go 语言开发，Fyne GUI 桌面界面。通过 UDP 广播自动发现局域网用户，TCP 直连收发消息，支持单聊和群聊，支持文字和图片。

## 常用命令

```bash
make build          # 本地构建到 build/xtx
make run            # 直接运行
make all            # 多架构交叉编译（darwin/linux/windows x amd64/arm64）
make tidy           # go mod tidy
go build ./...      # 编译检查
```

## 架构概览

```
main.go → 初始化 storage/discovery/chat → 启动 ui.App
```

- **internal/model/** — 数据结构和协议常量（User/ChatMessage/Group/BroadcastMsg）
- **internal/discovery/** — UDP 广播用户发现服务。处理上线/心跳/下线/群同步广播，维护在线用户表和群列表，通过 Events channel 向 UI 推送事件
- **internal/chat/** — TCP 消息收发服务。监听 TCP 端口接收 JSON 消息，提供 SendMessage（单发）和 SendToGroup（群发）
- **internal/storage/** — SQLite 本地持久化（modernc.org/sqlite 纯 Go）。存储聊天记录、群信息、用户设置
- **internal/ui/** — Fyne GUI 主界面。左侧用户/群列表，右侧聊天面板，消息输入和图片选择

## 通信协议

- **UDP 9527**：用户发现广播（ONLINE/HEARTBEAT/OFFLINE）和群聊同步（GROUP_CREATE/UPDATE/QUIT/SYNC）
- **TCP 9528**：聊天消息传输，JSON 格式，每条消息一个 TCP 连接
- 图片 <2MB 用 base64 内嵌，大图走独立连接

## 关键设计决策

- 纯 Go 无 CGO 依赖（sqlite 用 modernc.org/sqlite），方便交叉编译
- 群聊去中心化：群信息各节点本地存储，通过 UDP 广播最终一致
- 群消息由发送者负责 TCP 分发给每个成员
- 数据存储在 ~/.xtx/ 目录（SQLite 数据库 + 接收的图片）
