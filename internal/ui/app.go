package ui

import (
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/google/uuid"

	"github.com/ixx/xtx/internal/chat"
	"github.com/ixx/xtx/internal/discovery"
	"github.com/ixx/xtx/internal/model"
	db "github.com/ixx/xtx/internal/storage"
)

// session 代表一个聊天会话
type session struct {
	id       string // 单聊:对方IP, 群聊:群ID
	scope    string // private|group
	label    string // 显示名称
	messages []*model.StoredMessage
}

// App 主应用
type App struct {
	fyneApp    fyne.App
	window     fyne.Window
	discovery  *discovery.Service
	chatSvc    *chat.Service
	store      *db.DB

	sessions   map[string]*session
	currentSID string
	mu         sync.Mutex

	// UI组件
	userList       *widget.List
	chatHistory    *widget.List
	chatInput      *chatEntry
	chatTitleLabel *widget.Label
	sessionKeys    []string // 有序的会话key列表

	// 截图快捷键
	screenshotShortcut *desktop.CustomShortcut

	// 窗口焦点状态
	windowFocused bool

	// 左侧面板数据
	onlineUsers   []*model.User
	groups        []*model.Group
	sideItems     []sideItem // 合并后的列表
	sideFilter    string     // 用户搜索过滤关键词
	filteredItems []sideItem // 过滤后的列表

	// 文件传输状态
	pendingFiles map[string]string // fileID -> 本地文件路径（待发送）
	receivingFiles map[string]*receivingFile // fileID -> 接收中的文件
	fileMu       sync.Mutex

	// 侧边栏刷新防抖：合并 100ms 内的多次事件触发的刷新
	refreshMu    sync.Mutex
	refreshTimer *time.Timer
}

const sideRefreshDebounce = 100 * time.Millisecond

type receivingFile struct {
	filename   string
	fileSize   int64
	chunkTotal int
	received   map[int][]byte
	fromIP     string
	fromPort   int
	fromNick   string
	scope      string
	groupID    string
}

type sideItem struct {
	label   string
	id      string // IP or GroupID
	scope   string
	online  bool
	isGroup bool
}

// New 创建应用
func New(disc *discovery.Service, chatSvc *chat.Service, store *db.DB) *App {
	a := &App{
		fyneApp:        app.NewWithID("com.ixx.xtx"),
		discovery:      disc,
		chatSvc:        chatSvc,
		store:          store,
		sessions:       make(map[string]*session),
		pendingFiles:   make(map[string]string),
		receivingFiles: make(map[string]*receivingFile),
	}

	// 从存储加载并应用主题设置
	themeSetting, _ := store.GetSetting("theme")
	switch themeSetting {
	case "light":
		a.fyneApp.Settings().SetTheme(theme.LightTheme())
	case "dark":
		a.fyneApp.Settings().SetTheme(theme.DarkTheme())
	}

	a.window = a.fyneApp.NewWindow("XTX - 局域网聊天")
	a.window.Resize(fyne.NewSize(900, 600))
	return a
}

// SetIcon 设置应用图标（需在 Run 之前调用）
func (a *App) SetIcon(data []byte) {
	icon := fyne.NewStaticResource("logo.jpeg", data)
	a.fyneApp.SetIcon(icon)
}

// Run 启动应用
func (a *App) Run() {
	a.windowFocused = true
	a.buildUI()
	a.loadStoredGroups()

	// 跟踪窗口焦点
	a.fyneApp.Lifecycle().SetOnEnteredForeground(func() { a.windowFocused = true })
	a.fyneApp.Lifecycle().SetOnExitedForeground(func() { a.windowFocused = false })

	// 注册 Ctrl+Enter 快捷键（始终可用于发送）
	ctrlEnter := &desktop.CustomShortcut{KeyName: fyne.KeyReturn, Modifier: fyne.KeyModifierControl}
	a.window.Canvas().AddShortcut(ctrlEnter, func(fyne.Shortcut) {
		a.sendTextMessage()
	})

	// 注册截图快捷键
	a.registerScreenshotShortcut()

	go a.handleDiscoveryEvents()
	go a.handleIncomingMessages()
	go a.handleFileEvents()

	a.window.SetOnClosed(func() {
		a.discovery.Stop()
		a.chatSvc.Stop()
		a.store.Close()
	})

	a.window.ShowAndRun()
}

func (a *App) buildUI() {
	// --- 左侧面板 ---
	a.userList = widget.NewList(
		func() int {
			a.mu.Lock()
			defer a.mu.Unlock()
			return len(a.filteredItems)
		},
		func() fyne.CanvasObject {
			return container.NewHBox(
				widget.NewIcon(theme.AccountIcon()),
				widget.NewLabel("placeholder"),
			)
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			a.mu.Lock()
			if id >= len(a.filteredItems) {
				a.mu.Unlock()
				return
			}
			item := a.filteredItems[id]
			a.mu.Unlock()

			box := obj.(*fyne.Container)
			icon := box.Objects[0].(*widget.Icon)
			label := box.Objects[1].(*widget.Label)

			if item.isGroup {
				icon.SetResource(theme.MailComposeIcon())
			} else if item.online {
				icon.SetResource(theme.AccountIcon())
			} else {
				icon.SetResource(theme.VisibilityOffIcon())
			}
			label.SetText(item.label)
		},
	)
	a.userList.OnSelected = func(id widget.ListItemID) {
		a.mu.Lock()
		if id >= len(a.filteredItems) {
			a.mu.Unlock()
			return
		}
		item := a.filteredItems[id]
		a.mu.Unlock()
		a.switchSession(item.id, item.scope, item.label)
	}

	// 用户/群聊搜索框
	filterEntry := widget.NewEntry()
	filterEntry.SetPlaceHolder("搜索用户/群聊...")
	filterEntry.OnChanged = func(s string) {
		a.mu.Lock()
		a.sideFilter = s
		a.mu.Unlock()
		a.applyFilter()
		a.userList.Refresh()
	}

	createGroupBtn := widget.NewButton("+ 创建群聊", func() {
		a.showCreateGroupDialog()
	})

	refreshBtn := widget.NewButtonWithIcon("", theme.ViewRefreshIcon(), func() {
		a.discovery.Probe()
		a.refreshSidePanel()
	})

	settingsBtn := widget.NewButtonWithIcon("", theme.SettingsIcon(), func() {
		a.showSettingsDialog()
	})

	topBar := container.NewVBox(
		container.NewHBox(
			widget.NewLabel("在线用户 / 群聊"),
			layout.NewSpacer(),
			refreshBtn,
			settingsBtn,
		),
		filterEntry,
	)

	leftPanel := container.NewBorder(
		topBar,
		createGroupBtn,
		nil, nil,
		a.userList,
	)

	// --- 右侧面板 ---
	a.chatHistory = widget.NewList(
		func() int {
			a.mu.Lock()
			defer a.mu.Unlock()
			s := a.sessions[a.currentSID]
			if s == nil {
				return 0
			}
			return len(s.messages)
		},
		func() fyne.CanvasObject {
			nameLabel := widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
			contentLabel := widget.NewLabel("")
			contentLabel.Wrapping = fyne.TextWrapWord
			img := canvas.NewImageFromResource(nil)
			img.SetMinSize(fyne.NewSize(200, 150))
			img.FillMode = canvas.ImageFillContain
			img.Hidden = true
			imgBtn := widget.NewButton("", nil)
			imgBtn.Importance = widget.LowImportance
			imgBtn.Hidden = true
			imgContainer := container.NewStack(img, imgBtn)
			bubbleRect := canvas.NewRectangle(theme.Color(theme.ColorNameInputBackground))
			bubbleRect.CornerRadius = 8
			innerBox := container.NewPadded(container.NewVBox(nameLabel, contentLabel, imgContainer))
			bubble := container.NewStack(bubbleRect, innerBox)
			// 用 Border 布局实现左右对齐：bubble 居中，左右用 spacer 占位
			// 自己的消息靠右(leftSpacer可见)，对方消息靠左(rightSpacer可见)
			leftSpacer := widget.NewLabel("")
			rightSpacer := widget.NewLabel("")
			return container.NewBorder(nil, nil, leftSpacer, rightSpacer, bubble)
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			a.mu.Lock()
			s := a.sessions[a.currentSID]
			if s == nil || id >= len(s.messages) {
				a.mu.Unlock()
				return
			}
			msg := s.messages[id]
			localIP := a.discovery.LocalIP()
			a.mu.Unlock()

			// NewBorder puts center objects first, then left, right
			// So: Objects[0]=bubble, Objects[1]=leftSpacer, Objects[2]=rightSpacer
			borderContainer := obj.(*fyne.Container)
			bubble := borderContainer.Objects[0].(*fyne.Container)
			leftSpacer := borderContainer.Objects[1].(*widget.Label)
			rightSpacer := borderContainer.Objects[2].(*widget.Label)

			bubbleRect := bubble.Objects[0].(*canvas.Rectangle)
			paddedBox := bubble.Objects[1].(*fyne.Container)
			vbox := paddedBox.Objects[0].(*fyne.Container)
			nameLabel := vbox.Objects[0].(*widget.Label)
			contentLabel := vbox.Objects[1].(*widget.Label)
			imgContainer := vbox.Objects[2].(*fyne.Container)
			img := imgContainer.Objects[0].(*canvas.Image)
			imgBtn := imgContainer.Objects[1].(*widget.Button)

			// 气泡方向
			isMine := msg.FromIP == localIP
			if isMine {
				leftSpacer.Show()
				rightSpacer.Hide()
				bubbleRect.FillColor = theme.Color(theme.ColorNamePrimary)
			} else {
				leftSpacer.Hide()
				rightSpacer.Show()
				bubbleRect.FillColor = theme.Color(theme.ColorNameInputBackground)
			}
			bubbleRect.Refresh()

			t := time.Unix(msg.Timestamp, 0).Format("15:04")
			nameLabel.SetText(fmt.Sprintf("%s  %s", msg.FromNick, t))

			switch msg.Type {
			case model.ChatImage:
				contentLabel.Hidden = true
				img.Hidden = false
				imgBtn.Hidden = false
				imgContainer.Hidden = false
				if msg.Content != "" {
					img.File = msg.Content
					img.Refresh()
				}
				imgPath := msg.Content
				imgBtn.OnTapped = func() {
					a.showFullImage(imgPath)
				}
			case model.ChatFile:
				contentLabel.Hidden = false
				contentLabel.SetText("[文件] " + msg.Filename)
				img.Hidden = true
				imgBtn.Hidden = true
				imgContainer.Hidden = true
			default:
				contentLabel.Hidden = false
				contentLabel.SetText(msg.Content)
				img.Hidden = true
				imgBtn.Hidden = true
				imgContainer.Hidden = true
			}
		},
	)

	// 自定义聊天输入框
	a.chatInput = newChatEntry(func() { a.sendTextMessage() })
	a.chatInput.onPasteImage = a.tryPasteClipboardImage
	a.chatInput.SetMinRowsVisible(3)

	// 加载发送模式设置
	sendMode, _ := a.store.GetSetting("send_mode")
	a.chatInput.enterToSend = sendMode != "ctrl_enter"

	// 无边框主题
	entryTheme := &borderlessEntryTheme{parent: a.fyneApp.Settings().Theme()}
	entryContainer := container.NewThemeOverride(a.chatInput, entryTheme)

	sendBtn := widget.NewButtonWithIcon("发送", theme.MailSendIcon(), func() {
		a.sendTextMessage()
	})

	// 工具栏：小图标按钮
	fileBtn := widget.NewButtonWithIcon("", theme.FolderOpenIcon(), func() { a.sendFileRequest() })
	imgBtn := widget.NewButtonWithIcon("", theme.FileImageIcon(), func() { a.sendImageMessage() })
	emojiBtn := widget.NewButton("😀", func() { a.showEmojiPicker() })
	emojiBtn.Importance = widget.LowImportance
	screenshotBtn := widget.NewButtonWithIcon("", theme.ContentCutIcon(), func() { a.startScreenshot() })

	toolRow := container.NewHBox(fileBtn, imgBtn, emojiBtn, screenshotBtn)
	inputRow := container.NewBorder(nil, nil, nil, sendBtn, entryContainer)
	inputBar := container.NewVBox(toolRow, inputRow)

	chatTitle := widget.NewLabel("选择一个用户或群聊开始聊天")
	a.chatTitleLabel = chatTitle

	searchHistoryBtn := widget.NewButtonWithIcon("", theme.SearchIcon(), func() {
		a.showSearchDialog()
	})
	chatTitleBar := container.NewHBox(chatTitle, layout.NewSpacer(), searchHistoryBtn)

	rightPanel := container.NewBorder(
		chatTitleBar,
		inputBar,
		nil, nil,
		a.chatHistory,
	)

	// --- 主布局 ---
	split := container.NewHSplit(leftPanel, rightPanel)
	split.SetOffset(0.25)

	a.window.SetContent(split)
}

func (a *App) loadStoredGroups() {
	groups, err := a.store.LoadGroups()
	if err != nil {
		log.Printf("加载群聊失败: %v", err)
		return
	}
	for _, g := range groups {
		a.discovery.AddGroup(g)
	}
	a.refreshSidePanel()
}

func (a *App) handleDiscoveryEvents() {
	for evt := range a.discovery.Events() {
		switch evt.Type {
		case discovery.EventUserOnline, discovery.EventUserOffline:
			a.scheduleRefreshSidePanel()
		case discovery.EventGroupCreated, discovery.EventGroupUpdated:
			if evt.Group != nil {
				_ = a.store.SaveGroup(evt.Group)
			}
			a.scheduleRefreshSidePanel()
		case discovery.EventGroupQuit:
			if evt.Group != nil {
				if evt.IP == a.discovery.LocalIP() {
					_ = a.store.DeleteGroup(evt.Group.ID)
				} else {
					_ = a.store.SaveGroup(evt.Group)
				}
			}
			a.scheduleRefreshSidePanel()
		}
	}
}

func (a *App) handleIncomingMessages() {
	for msg := range a.chatSvc.Incoming() {
		var sessionID string
		if msg.Scope == model.ScopeGroup {
			sessionID = msg.GroupID
		} else {
			sessionID = model.UserKey(msg.FromIP, msg.FromPort)
		}

		// 保存图片到本地
		filename := msg.Filename
		if msg.Type == model.ChatImage && msg.Content != "" {
			imgData, err := base64.StdEncoding.DecodeString(msg.Content)
			if err == nil {
				imgPath := filepath.Join(a.store.ImageDir(), fmt.Sprintf("%d_%s", msg.Timestamp, filename))
				os.WriteFile(imgPath, imgData, 0644)
				msg.Content = imgPath // 存储路径而非base64
			}
		}

		stored := &model.StoredMessage{
			SessionID: sessionID,
			Scope:     msg.Scope,
			FromNick:  msg.From,
			FromIP:    msg.FromIP,
			Type:      msg.Type,
			Content:   msg.Content,
			Filename:  filename,
			Timestamp: msg.Timestamp,
		}
		_ = a.store.SaveMessage(stored)

		a.mu.Lock()
		s, ok := a.sessions[sessionID]
		if !ok {
			label := msg.From
			if msg.Scope == model.ScopeGroup {
				if g := a.discovery.GetGroup(msg.GroupID); g != nil {
					label = g.Name
				}
			}
			s = &session{id: sessionID, scope: msg.Scope, label: label}
			a.sessions[sessionID] = s
		}
		s.messages = append(s.messages, stored)
		isCurrentSession := a.currentSID == sessionID
		a.mu.Unlock()

		if isCurrentSession {
			a.chatHistory.Refresh()
			a.chatHistory.ScrollToBottom()
		}

		// 消息通知
		if !a.windowFocused || !isCurrentSession {
			var body string
			switch msg.Type {
			case model.ChatImage:
				body = "[图片]"
			case model.ChatFile, model.ChatFileRequest:
				body = "[文件]"
			default:
				body = msg.Content
			}
			a.fyneApp.SendNotification(fyne.NewNotification(msg.From, body))
		}

		a.scheduleRefreshSidePanel()
	}
}

// scheduleRefreshSidePanel 在防抖窗口内合并多次刷新，适合高频事件路径。
// 首次调用时立即安排 100ms 后执行，窗口内其它调用被吞掉。
func (a *App) scheduleRefreshSidePanel() {
	a.refreshMu.Lock()
	if a.refreshTimer != nil {
		a.refreshMu.Unlock()
		return
	}
	a.refreshTimer = time.AfterFunc(sideRefreshDebounce, func() {
		a.refreshMu.Lock()
		a.refreshTimer = nil
		a.refreshMu.Unlock()
		a.refreshSidePanel()
	})
	a.refreshMu.Unlock()
}

func (a *App) refreshSidePanel() {
	users := a.discovery.GetUsers()
	groups := a.discovery.GetGroups()

	// 排序：在线优先，然后按昵称
	sort.Slice(users, func(i, j int) bool {
		if users[i].Online != users[j].Online {
			return users[i].Online
		}
		return users[i].Nickname < users[j].Nickname
	})
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].Name < groups[j].Name
	})

	items := make([]sideItem, 0, len(users)+len(groups)+1)
	for _, u := range users {
		label := u.Nickname
		if !u.Online {
			label += " (离线)"
		}
		items = append(items, sideItem{
			label:   label,
			id:      u.Key(),
			scope:   model.ScopePrivate,
			online:  u.Online,
			isGroup: false,
		})
	}
	if len(groups) > 0 {
		items = append(items, sideItem{label: "── 群聊 ──", id: "_sep", scope: "_sep"})
		for _, g := range groups {
			items = append(items, sideItem{
				label:   fmt.Sprintf("[群] %s (%d人)", g.Name, len(g.Members)),
				id:      g.ID,
				scope:   model.ScopeGroup,
				isGroup: true,
			})
		}
	}

	a.mu.Lock()
	a.onlineUsers = users
	a.groups = groups
	a.sideItems = items
	a.mu.Unlock()

	a.applyFilter()
	a.userList.Refresh()
}

func (a *App) switchSession(id, scope, label string) {
	if id == "_sep" {
		return
	}
	a.mu.Lock()
	s, ok := a.sessions[id]
	if !ok {
		s = &session{id: id, scope: scope, label: label}
		a.sessions[id] = s
		// 从数据库加载历史
		msgs, err := a.store.LoadMessages(id, 200)
		if err == nil {
			s.messages = msgs
		}
	}
	a.currentSID = id
	a.mu.Unlock()

	a.chatTitleLabel.SetText(fmt.Sprintf("与 %s 的对话", label))
	a.chatHistory.Refresh()
	if len(s.messages) > 0 {
		a.chatHistory.ScrollToBottom()
	}
}

func (a *App) sendTextMessage() {
	text := strings.TrimSpace(a.chatInput.Text)
	if text == "" {
		return
	}
	a.mu.Lock()
	sid := a.currentSID
	s := a.sessions[sid]
	a.mu.Unlock()
	if s == nil {
		return
	}

	now := time.Now().Unix()
	chatMsg := &model.ChatMessage{
		Type:      model.ChatText,
		Scope:     s.scope,
		GroupID:   "",
		From:      a.discovery.Nickname(),
		FromIP:    a.discovery.LocalIP(),
		FromPort:  a.discovery.TCPPort(),
		Timestamp: now,
		Content:   text,
	}
	if s.scope == model.ScopeGroup {
		chatMsg.GroupID = s.id
	}

	// 发送
	if err := a.doSend(s, chatMsg); err != nil {
		dialog.ShowError(fmt.Errorf("发送失败: %v", err), a.window)
		return
	}

	// 本地保存
	stored := &model.StoredMessage{
		SessionID: s.id,
		Scope:     s.scope,
		FromNick:  a.discovery.Nickname(),
		FromIP:    a.discovery.LocalIP(),
		Type:      model.ChatText,
		Content:   text,
		Timestamp: now,
	}
	_ = a.store.SaveMessage(stored)

	a.mu.Lock()
	s.messages = append(s.messages, stored)
	a.mu.Unlock()

	a.chatInput.SetText("")
	a.chatHistory.Refresh()
	a.chatHistory.ScrollToBottom()
}

func (a *App) sendImageMessage() {
	a.mu.Lock()
	s := a.sessions[a.currentSID]
	a.mu.Unlock()
	if s == nil {
		return
	}

	dlg := dialog.NewFileOpen(func(reader fyne.URIReadCloser, err error) {
		if err != nil || reader == nil {
			return
		}
		defer reader.Close()

		data, err := readAll(reader)
		if err != nil {
			dialog.ShowError(fmt.Errorf("读取图片失败: %v", err), a.window)
			return
		}

		filename := filepath.Base(reader.URI().Path())
		b64 := base64.StdEncoding.EncodeToString(data)

		now := time.Now().Unix()
		chatMsg := &model.ChatMessage{
			Type:      model.ChatImage,
			Scope:     s.scope,
			From:      a.discovery.Nickname(),
			FromIP:    a.discovery.LocalIP(),
			FromPort:  a.discovery.TCPPort(),
			Timestamp: now,
			Content:   b64,
			Filename:  filename,
		}
		if s.scope == model.ScopeGroup {
			chatMsg.GroupID = s.id
		}

		if err := a.doSend(s, chatMsg); err != nil {
			dialog.ShowError(fmt.Errorf("发送失败: %v", err), a.window)
			return
		}

		// 保存本地
		imgPath := filepath.Join(a.store.ImageDir(), fmt.Sprintf("%d_%s", now, filename))
		os.WriteFile(imgPath, data, 0644)

		stored := &model.StoredMessage{
			SessionID: s.id,
			Scope:     s.scope,
			FromNick:  a.discovery.Nickname(),
			FromIP:    a.discovery.LocalIP(),
			Type:      model.ChatImage,
			Content:   imgPath,
			Filename:  filename,
			Timestamp: now,
		}
		_ = a.store.SaveMessage(stored)

		a.mu.Lock()
		s.messages = append(s.messages, stored)
		a.mu.Unlock()

		a.chatHistory.Refresh()
		a.chatHistory.ScrollToBottom()
	}, a.window)

	dlg.SetFilter(storage.NewExtensionFileFilter([]string{".png", ".jpg", ".jpeg", ".gif", ".bmp", ".webp"}))
	dlg.Show()
}

func (a *App) doSend(s *session, msg *model.ChatMessage) error {
	if s.scope == model.ScopePrivate {
		user := a.discovery.GetUserByKey(s.id)
		if user == nil || !user.Online {
			return fmt.Errorf("用户不在线")
		}
		return a.chatSvc.SendMessage(user.IP, user.TCPPort, msg)
	}
	// 群聊
	g := a.discovery.GetGroup(s.id)
	if g == nil {
		return fmt.Errorf("群聊不存在")
	}
	var members []model.User
	for _, ip := range g.Members {
		if u := a.discovery.GetUserByIP(ip); u != nil {
			members = append(members, *u)
		}
	}
	errs := a.chatSvc.SendToGroup(members, msg, model.UserKey(a.discovery.LocalIP(), a.discovery.TCPPort()))
	if len(errs) > 0 {
		return fmt.Errorf("部分成员发送失败: %d", len(errs))
	}
	return nil
}

func (a *App) showCreateGroupDialog() {
	users := a.discovery.GetUsers()
	var onlineUsers []*model.User
	for _, u := range users {
		if u.Online {
			onlineUsers = append(onlineUsers, u)
		}
	}
	if len(onlineUsers) == 0 {
		dialog.ShowInformation("提示", "当前没有在线用户", a.window)
		return
	}

	nameEntry := widget.NewEntry()
	nameEntry.SetPlaceHolder("群聊名称")

	checks := make([]*widget.Check, len(onlineUsers))
	checkContainer := container.NewVBox()
	for i, u := range onlineUsers {
		checks[i] = widget.NewCheck(fmt.Sprintf("%s (%s)", u.Nickname, u.IP), nil)
		checkContainer.Add(checks[i])
	}

	content := container.NewVBox(
		widget.NewLabel("群名称:"),
		nameEntry,
		widget.NewLabel("选择成员:"),
		container.NewVScroll(checkContainer),
	)

	dlg := dialog.NewCustomConfirm("创建群聊", "创建", "取消", content, func(ok bool) {
		if !ok {
			return
		}
		name := strings.TrimSpace(nameEntry.Text)
		if name == "" {
			name = "新群聊"
		}

		members := []string{a.discovery.LocalIP()}
		for i, c := range checks {
			if c.Checked {
				members = append(members, onlineUsers[i].IP)
			}
		}
		if len(members) < 2 {
			dialog.ShowInformation("提示", "请至少选择一个成员", a.window)
			return
		}

		g := &model.Group{
			ID:        uuid.New().String(),
			Name:      name,
			Members:   members,
			CreatorIP: a.discovery.LocalIP(),
		}

		a.discovery.AddGroup(g)
		_ = a.store.SaveGroup(g)
		a.discovery.BroadcastGroupCreate(g)
		a.refreshSidePanel()
		a.switchSession(g.ID, model.ScopeGroup, g.Name)
	}, a.window)

	dlg.Resize(fyne.NewSize(400, 400))
	dlg.Show()
}

func readAll(r fyne.URIReadCloser) ([]byte, error) {
	var buf []byte
	tmp := make([]byte, 32*1024)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			return buf, err
		}
	}
	return buf, nil
}

// ========== 文件传输 ==========

const fileChunkSize = 512 * 1024 // 512KB per chunk

func (a *App) sendFileRequest() {
	a.mu.Lock()
	s := a.sessions[a.currentSID]
	a.mu.Unlock()
	if s == nil {
		return
	}

	dlg := dialog.NewFileOpen(func(reader fyne.URIReadCloser, err error) {
		if err != nil || reader == nil {
			return
		}
		reader.Close()

		filePath := reader.URI().Path()
		info, err := os.Stat(filePath)
		if err != nil {
			dialog.ShowError(fmt.Errorf("读取文件信息失败: %v", err), a.window)
			return
		}

		fileID := uuid.New().String()
		filename := filepath.Base(filePath)

		a.fileMu.Lock()
		a.pendingFiles[fileID] = filePath
		a.fileMu.Unlock()

		now := time.Now().Unix()
		chatMsg := &model.ChatMessage{
			Type:      model.ChatFileRequest,
			Scope:     s.scope,
			From:      a.discovery.Nickname(),
			FromIP:    a.discovery.LocalIP(),
			FromPort:  a.discovery.TCPPort(),
			Timestamp: now,
			Content:   filename,
			Filename:  filename,
			FileID:    fileID,
			FileSize:  info.Size(),
		}
		if s.scope == model.ScopeGroup {
			chatMsg.GroupID = s.id
		}

		if err := a.doSend(s, chatMsg); err != nil {
			dialog.ShowError(fmt.Errorf("发送文件请求失败: %v", err), a.window)
			a.fileMu.Lock()
			delete(a.pendingFiles, fileID)
			a.fileMu.Unlock()
		}
	}, a.window)
	dlg.Show()
}

func (a *App) handleFileEvents() {
	for evt := range a.chatSvc.FileEvents() {
		msg := evt.Msg
		switch msg.Type {
		case model.ChatFileRequest:
			a.handleFileRequest(msg)
		case model.ChatFileAccept:
			a.handleFileAccept(msg)
		case model.ChatFileReject:
			a.handleFileReject(msg)
		case model.ChatFileData:
			a.handleFileData(msg)
		case model.ChatFileComplete:
			a.handleFileComplete(msg)
		}
	}
}

func (a *App) handleFileRequest(msg *model.ChatMessage) {
	sizeStr := formatFileSize(msg.FileSize)
	title := fmt.Sprintf("%s 想发送文件", msg.From)
	text := fmt.Sprintf("%s (%s)，是否接受？", msg.Filename, sizeStr)

	dialog.ShowConfirm(title, text, func(accept bool) {
		reply := &model.ChatMessage{
			Scope:     msg.Scope,
			GroupID:   msg.GroupID,
			From:      a.discovery.Nickname(),
			FromIP:    a.discovery.LocalIP(),
			FromPort:  a.discovery.TCPPort(),
			Timestamp: time.Now().Unix(),
			FileID:    msg.FileID,
			Filename:  msg.Filename,
		}

		if accept {
			reply.Type = model.ChatFileAccept
			// 准备接收
			a.fileMu.Lock()
			chunkTotal := int(msg.FileSize / fileChunkSize)
			if msg.FileSize%fileChunkSize != 0 {
				chunkTotal++
			}
			a.receivingFiles[msg.FileID] = &receivingFile{
				filename:   msg.Filename,
				fileSize:   msg.FileSize,
				chunkTotal: chunkTotal,
				received:   make(map[int][]byte),
				fromIP:     msg.FromIP,
				fromPort:   msg.FromPort,
				fromNick:   msg.From,
				scope:      msg.Scope,
				groupID:    msg.GroupID,
			}
			a.fileMu.Unlock()
		} else {
			reply.Type = model.ChatFileReject
		}

		user := a.discovery.GetUserByKey(model.UserKey(msg.FromIP, msg.FromPort))
		if user != nil {
			_ = a.chatSvc.SendMessage(user.IP, user.TCPPort, reply)
		}
	}, a.window)
}

func (a *App) handleFileAccept(msg *model.ChatMessage) {
	a.fileMu.Lock()
	filePath, ok := a.pendingFiles[msg.FileID]
	a.fileMu.Unlock()
	if !ok {
		return
	}

	go a.sendFileChunks(model.UserKey(msg.FromIP, msg.FromPort), msg.FileID, filePath, msg.Scope, msg.GroupID)
}

func (a *App) handleFileReject(msg *model.ChatMessage) {
	a.fileMu.Lock()
	delete(a.pendingFiles, msg.FileID)
	a.fileMu.Unlock()

	dialog.ShowInformation("文件传输", fmt.Sprintf("%s 拒绝了文件接收", msg.From), a.window)
}

func (a *App) sendFileChunks(targetKey, fileID, filePath, scope, groupID string) {
	defer func() {
		a.fileMu.Lock()
		delete(a.pendingFiles, fileID)
		a.fileMu.Unlock()
	}()

	data, err := os.ReadFile(filePath)
	if err != nil {
		log.Printf("读取文件失败: %v", err)
		return
	}

	filename := filepath.Base(filePath)
	chunkTotal := len(data) / fileChunkSize
	if len(data)%fileChunkSize != 0 {
		chunkTotal++
	}

	user := a.discovery.GetUserByKey(targetKey)
	if user == nil {
		return
	}

	for i := 0; i < chunkTotal; i++ {
		start := i * fileChunkSize
		end := start + fileChunkSize
		if end > len(data) {
			end = len(data)
		}

		chunk := base64.StdEncoding.EncodeToString(data[start:end])
		chunkMsg := &model.ChatMessage{
			Type:       model.ChatFileData,
			Scope:      scope,
			GroupID:    groupID,
			From:       a.discovery.Nickname(),
			FromIP:     a.discovery.LocalIP(),
			FromPort:   a.discovery.TCPPort(),
			Timestamp:  time.Now().Unix(),
			FileID:     fileID,
			Filename:   filename,
			Content:    chunk,
			ChunkIdx:   i,
			ChunkTotal: chunkTotal,
		}

		if err := a.chatSvc.SendMessage(user.IP, user.TCPPort, chunkMsg); err != nil {
			log.Printf("发送文件块 %d/%d 失败: %v", i+1, chunkTotal, err)
			return
		}
	}

	// 发送完成确认
	completeMsg := &model.ChatMessage{
		Type:      model.ChatFileComplete,
		Scope:     scope,
		GroupID:   groupID,
		From:      a.discovery.Nickname(),
		FromIP:    a.discovery.LocalIP(),
		FromPort:  a.discovery.TCPPort(),
		Timestamp: time.Now().Unix(),
		FileID:    fileID,
		Filename:  filename,
	}
	_ = a.chatSvc.SendMessage(user.IP, user.TCPPort, completeMsg)

	// 发送方也在聊天记录中显示
	sessionID := targetKey
	if scope == model.ScopeGroup {
		sessionID = groupID
	}
	stored := &model.StoredMessage{
		SessionID: sessionID,
		Scope:     scope,
		FromNick:  a.discovery.Nickname(),
		FromIP:    a.discovery.LocalIP(),
		Type:      model.ChatFile,
		Content:   filePath,
		Filename:  filename,
		Timestamp: time.Now().Unix(),
	}
	_ = a.store.SaveMessage(stored)

	a.mu.Lock()
	if s, ok := a.sessions[sessionID]; ok {
		s.messages = append(s.messages, stored)
	}
	isCurrentSession := a.currentSID == sessionID
	a.mu.Unlock()

	if isCurrentSession {
		a.chatHistory.Refresh()
		a.chatHistory.ScrollToBottom()
	}
}

func (a *App) handleFileData(msg *model.ChatMessage) {
	a.fileMu.Lock()
	rf, ok := a.receivingFiles[msg.FileID]
	if !ok {
		a.fileMu.Unlock()
		return
	}

	data, err := base64.StdEncoding.DecodeString(msg.Content)
	if err != nil {
		a.fileMu.Unlock()
		log.Printf("解码文件块失败: %v", err)
		return
	}
	rf.received[msg.ChunkIdx] = data
	a.fileMu.Unlock()
}

func (a *App) handleFileComplete(msg *model.ChatMessage) {
	a.fileMu.Lock()
	rf, ok := a.receivingFiles[msg.FileID]
	if !ok {
		a.fileMu.Unlock()
		return
	}
	delete(a.receivingFiles, msg.FileID)
	a.fileMu.Unlock()

	// 拼接所有块
	var fileData []byte
	for i := 0; i < rf.chunkTotal; i++ {
		chunk, exists := rf.received[i]
		if !exists {
			log.Printf("文件块 %d 缺失", i)
			return
		}
		fileData = append(fileData, chunk...)
	}

	// 写入文件
	destPath := filepath.Join(a.store.FileDir(), fmt.Sprintf("%d_%s", time.Now().Unix(), rf.filename))
	if err := os.WriteFile(destPath, fileData, 0644); err != nil {
		log.Printf("写入文件失败: %v", err)
		return
	}

	// 保存聊天记录
	sessionID := model.UserKey(rf.fromIP, rf.fromPort)
	if rf.scope == model.ScopeGroup {
		sessionID = rf.groupID
	}
	stored := &model.StoredMessage{
		SessionID: sessionID,
		Scope:     rf.scope,
		FromNick:  rf.fromNick,
		FromIP:    rf.fromIP,
		Type:      model.ChatFile,
		Content:   destPath,
		Filename:  rf.filename,
		Timestamp: time.Now().Unix(),
	}
	_ = a.store.SaveMessage(stored)

	a.mu.Lock()
	s, exists := a.sessions[sessionID]
	if !exists {
		s = &session{id: sessionID, scope: rf.scope, label: rf.fromNick}
		a.sessions[sessionID] = s
	}
	s.messages = append(s.messages, stored)
	isCurrentSession := a.currentSID == sessionID
	a.mu.Unlock()

	if isCurrentSession {
		a.chatHistory.Refresh()
		a.chatHistory.ScrollToBottom()
	}
	a.scheduleRefreshSidePanel()
}

func formatFileSize(size int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)
	switch {
	case size >= GB:
		return fmt.Sprintf("%.1f GB", float64(size)/float64(GB))
	case size >= MB:
		return fmt.Sprintf("%.1f MB", float64(size)/float64(MB))
	case size >= KB:
		return fmt.Sprintf("%.1f KB", float64(size)/float64(KB))
	default:
		return fmt.Sprintf("%d B", size)
	}
}

// ========== 图片发送辅助 ==========

func (a *App) sendImageFromPath(imgPath string) {
	a.mu.Lock()
	s := a.sessions[a.currentSID]
	a.mu.Unlock()
	if s == nil {
		return
	}

	data, err := os.ReadFile(imgPath)
	if err != nil {
		return
	}

	filename := filepath.Base(imgPath)
	b64 := base64.StdEncoding.EncodeToString(data)

	now := time.Now().Unix()
	chatMsg := &model.ChatMessage{
		Type:      model.ChatImage,
		Scope:     s.scope,
		From:      a.discovery.Nickname(),
		FromIP:    a.discovery.LocalIP(),
		FromPort:  a.discovery.TCPPort(),
		Timestamp: now,
		Content:   b64,
		Filename:  filename,
	}
	if s.scope == model.ScopeGroup {
		chatMsg.GroupID = s.id
	}

	if err := a.doSend(s, chatMsg); err != nil {
		dialog.ShowError(fmt.Errorf("发送失败: %v", err), a.window)
		return
	}

	localPath := filepath.Join(a.store.ImageDir(), fmt.Sprintf("%d_%s", now, filename))
	_ = os.WriteFile(localPath, data, 0644)

	stored := &model.StoredMessage{
		SessionID: s.id,
		Scope:     s.scope,
		FromNick:  a.discovery.Nickname(),
		FromIP:    a.discovery.LocalIP(),
		Type:      model.ChatImage,
		Content:   localPath,
		Filename:  filename,
		Timestamp: now,
	}
	_ = a.store.SaveMessage(stored)

	a.mu.Lock()
	s.messages = append(s.messages, stored)
	a.mu.Unlock()

	a.chatHistory.Refresh()
	a.chatHistory.ScrollToBottom()
}

func (a *App) tryPasteClipboardImage() bool {
	tmpPath := filepath.Join(os.TempDir(), fmt.Sprintf("xtx_paste_%d.png", time.Now().UnixNano()))
	if err := clipboardReadImage(tmpPath); err != nil {
		return false
	}
	defer os.Remove(tmpPath)
	a.sendImageFromPath(tmpPath)
	return true
}

func (a *App) registerScreenshotShortcut() {
	// 移除旧快捷键
	if a.screenshotShortcut != nil {
		a.window.Canvas().RemoveShortcut(a.screenshotShortcut)
		a.screenshotShortcut = nil
	}

	hotkey, _ := a.store.GetSetting("screenshot_hotkey")
	if hotkey == "" {
		hotkey = "ctrl+shift+a"
	}

	s := parseShortcut(hotkey)
	if s != nil {
		a.screenshotShortcut = s
		a.window.Canvas().AddShortcut(s, func(fyne.Shortcut) {
			a.startScreenshot()
		})
	}
}

// ========== 设置对话框 ==========

func (a *App) showSettingsDialog() {
	// Tab 1: 基本设置
	nicknameEntry := widget.NewEntry()
	nicknameEntry.SetText(a.discovery.Nickname())

	// 发送模式
	sendModeOpts := []string{"Enter 发送", "Ctrl+Enter 发送"}
	sendModeSelect := widget.NewSelect(sendModeOpts, nil)
	currentSendMode, _ := a.store.GetSetting("send_mode")
	if currentSendMode == "ctrl_enter" {
		sendModeSelect.SetSelected("Ctrl+Enter 发送")
	} else {
		sendModeSelect.SetSelected("Enter 发送")
	}

	// 截图快捷键
	hotkeyOpts := []string{"Ctrl+Shift+A", "Ctrl+Shift+S", "Ctrl+Shift+X", "Ctrl+Alt+A"}
	hotkeySelect := widget.NewSelect(hotkeyOpts, nil)
	currentHotkey, _ := a.store.GetSetting("screenshot_hotkey")
	if currentHotkey == "" {
		currentHotkey = "ctrl+shift+a"
	}
	for _, opt := range hotkeyOpts {
		if strings.EqualFold(opt, strings.ReplaceAll(currentHotkey, "+", "+")) {
			hotkeySelect.SetSelected(opt)
		}
	}
	if hotkeySelect.Selected == "" {
		hotkeySelect.SetSelected(hotkeyOpts[0])
	}

	basicTab := container.NewVBox(
		widget.NewLabel("昵称:"),
		nicknameEntry,
		widget.NewLabel("发送方式:"),
		sendModeSelect,
		widget.NewLabel("截图快捷键:"),
		hotkeySelect,
	)

	// Tab 2: 网络设置
	extraAddrs, _ := a.store.GetSetting("extra_broadcast")
	addrsEntry := widget.NewMultiLineEntry()
	addrsEntry.SetPlaceHolder("每行一个广播地址，如 192.168.2.255")
	addrsEntry.SetText(extraAddrs)
	addrsEntry.SetMinRowsVisible(4)
	networkTab := container.NewVBox(
		widget.NewLabel("额外广播地址:"),
		addrsEntry,
	)

	// Tab 3: 外观设置
	currentTheme, _ := a.store.GetSetting("theme")
	themeOptions := []string{"跟随系统", "亮色", "暗色"}
	themeSelect := widget.NewSelect(themeOptions, nil)
	switch currentTheme {
	case "light":
		themeSelect.SetSelected("亮色")
	case "dark":
		themeSelect.SetSelected("暗色")
	default:
		themeSelect.SetSelected("跟随系统")
	}
	appearanceTab := container.NewVBox(
		widget.NewLabel("主题:"),
		themeSelect,
	)

	tabs := container.NewAppTabs(
		container.NewTabItem("基本", basicTab),
		container.NewTabItem("网络", networkTab),
		container.NewTabItem("外观", appearanceTab),
	)

	dlg := dialog.NewCustomConfirm("设置", "保存", "取消", tabs, func(ok bool) {
		if !ok {
			return
		}

		// 保存昵称
		newNick := strings.TrimSpace(nicknameEntry.Text)
		if newNick != "" && newNick != a.discovery.Nickname() {
			_ = a.store.SetSetting("nickname", newNick)
			a.discovery.SetNickname(newNick)
		}

		// 保存额外广播地址
		addrText := strings.TrimSpace(addrsEntry.Text)
		_ = a.store.SetSetting("extra_broadcast", addrText)
		var addrs []string
		if addrText != "" {
			for _, line := range strings.Split(addrText, "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					addrs = append(addrs, line)
				}
			}
		}
		a.discovery.SetExtraBroadcastAddrs(addrs)

		// 保存发送模式
		if sendModeSelect.Selected == "Ctrl+Enter 发送" {
			_ = a.store.SetSetting("send_mode", "ctrl_enter")
			a.chatInput.enterToSend = false
		} else {
			_ = a.store.SetSetting("send_mode", "enter")
			a.chatInput.enterToSend = true
		}

		// 保存截图快捷键
		newHotkey := strings.ToLower(hotkeySelect.Selected)
		_ = a.store.SetSetting("screenshot_hotkey", newHotkey)
		a.registerScreenshotShortcut()

		// 保存主题
		var themeVal string
		switch themeSelect.Selected {
		case "亮色":
			themeVal = "light"
			a.fyneApp.Settings().SetTheme(theme.LightTheme())
		case "暗色":
			themeVal = "dark"
			a.fyneApp.Settings().SetTheme(theme.DarkTheme())
		default:
			themeVal = ""
			a.fyneApp.Settings().SetTheme(theme.DefaultTheme())
		}
		_ = a.store.SetSetting("theme", themeVal)
	}, a.window)

	dlg.Resize(fyne.NewSize(450, 350))
	dlg.Show()
}

// applyFilter 根据 sideFilter 过滤 sideItems
func (a *App) applyFilter() {
	a.mu.Lock()
	defer a.mu.Unlock()

	keyword := strings.ToLower(a.sideFilter)
	if keyword == "" {
		a.filteredItems = a.sideItems
		return
	}

	filtered := make([]sideItem, 0)
	for _, item := range a.sideItems {
		if item.id == "_sep" {
			continue
		}
		if strings.Contains(strings.ToLower(item.label), keyword) {
			filtered = append(filtered, item)
		}
	}
	a.filteredItems = filtered
}

// showSearchDialog 显示历史记录搜索窗口
func (a *App) showSearchDialog() {
	w := a.fyneApp.NewWindow("搜索聊天记录")
	w.Resize(fyne.NewSize(500, 400))

	var results []*model.StoredMessage
	var resultsMu sync.Mutex

	resultList := widget.NewList(
		func() int {
			resultsMu.Lock()
			defer resultsMu.Unlock()
			return len(results)
		},
		func() fyne.CanvasObject {
			from := widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
			content := widget.NewLabel("")
			content.Wrapping = fyne.TextWrapWord
			return container.NewVBox(from, content)
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			resultsMu.Lock()
			if id >= len(results) {
				resultsMu.Unlock()
				return
			}
			msg := results[id]
			resultsMu.Unlock()

			box := obj.(*fyne.Container)
			from := box.Objects[0].(*widget.Label)
			content := box.Objects[1].(*widget.Label)

			t := time.Unix(msg.Timestamp, 0).Format("2006-01-02 15:04")
			from.SetText(fmt.Sprintf("%s  %s", msg.FromNick, t))

			preview := msg.Content
			if len(preview) > 100 {
				preview = preview[:100] + "..."
			}
			content.SetText(preview)
		},
	)

	resultList.OnSelected = func(id widget.ListItemID) {
		resultsMu.Lock()
		if id >= len(results) {
			resultsMu.Unlock()
			return
		}
		msg := results[id]
		resultsMu.Unlock()

		// 切换到对应会话
		label := msg.FromNick
		scope := msg.Scope
		sessionID := msg.SessionID
		if scope == model.ScopeGroup {
			if g := a.discovery.GetGroup(sessionID); g != nil {
				label = g.Name
			}
		}
		a.switchSession(sessionID, scope, label)
		w.Close()
	}

	searchEntry := widget.NewEntry()
	searchEntry.SetPlaceHolder("输入关键词搜索...")
	searchEntry.OnChanged = func(kw string) {
		kw = strings.TrimSpace(kw)
		if len(kw) < 2 {
			resultsMu.Lock()
			results = nil
			resultsMu.Unlock()
			resultList.Refresh()
			return
		}
		msgs, err := a.store.SearchMessages(kw, 50)
		if err != nil {
			return
		}
		resultsMu.Lock()
		results = msgs
		resultsMu.Unlock()
		resultList.Refresh()
	}

	content := container.NewBorder(searchEntry, nil, nil, nil, resultList)
	w.SetContent(content)
	w.Show()
}

// showFullImage 在新窗口查看原图
func (a *App) showFullImage(imgPath string) {
	w := a.fyneApp.NewWindow("查看图片")
	w.Resize(fyne.NewSize(800, 600))

	img := canvas.NewImageFromFile(imgPath)
	img.FillMode = canvas.ImageFillContain

	w.SetContent(img)
	w.Show()
}

