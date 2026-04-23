package ui

import (
	"encoding/base64"
	"fmt"
	"image/color"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	selfInfoLabel  *widget.Label // 左侧底部"当前用户"信息
	sessionKeys    []string      // 有序的会话key列表

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
	receivingFiles map[string]*receivingFile    // fileID -> 已 accept 正在接收
	fileRequests   map[string]*fileRequestState // fileID -> 待确认的请求（未 accept/reject/timeout）
	fileMu         sync.Mutex

	// 侧边栏刷新防抖：合并 100ms 内的多次事件触发的刷新
	refreshMu    sync.Mutex
	refreshTimer *time.Timer

	// 未读消息计数：sessionID -> 未读条数。当前会话/窗口聚焦时不计数
	unreadCounts map[string]int
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
	storedID   int64 // 对应的 StoredMessage ID，完成时更新
}

// fileRequestState 跟踪一次"待确认"的文件请求（发送方与接收方各一份）。
// 在存储表里对应一条 Type=ChatFileRequest 的消息，所有状态流转都通过
// storedMsg 指针就地修改并回写 DB。
type fileRequestState struct {
	fileID    string
	storedMsg *model.StoredMessage // 指向 session.messages 里的那条
	sessionID string
	timer     *time.Timer

	// 发送方专用
	localPath string
	toKey     string // 目标 UserKey（单聊用）
	scope     string
	groupID   string

	// 接收方专用
	fromIP   string
	fromPort int
	fromNick string
	fileSize int64
}

const fileRequestTimeout = 5 * time.Minute

type sideItem struct {
	label    string // 仅用于搜索匹配
	name     string // 主显示（昵称或群名）
	subtitle string // 副标题（IP / "N 人" / "离线"）
	id       string // IP:port 或 GroupID 或 "_sep"
	scope    string
	online   bool
	isGroup  bool
	isSep    bool
	unread   int
}

// New 创建应用
func New(disc *discovery.Service, chatSvc *chat.Service, store *db.DB) *App {
	a := &App{
		fyneApp:        app.NewWithID("com.ixx.xtx"),
		discovery:      disc,
		chatSvc:        chatSvc,
		store:          store,
		sessions:       make(map[string]*session),
		receivingFiles: make(map[string]*receivingFile),
		fileRequests:   make(map[string]*fileRequestState),
		unreadCounts:   make(map[string]int),
	}

	// 从存储加载并应用主题设置（统一包一层 appTheme，保证输入框无描边）
	themeSetting, _ := store.GetSetting("theme")
	a.fyneApp.Settings().SetTheme(wrapBaseTheme(themeSetting))

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

	// 窗口失焦跟踪（聚焦回调在 SetOnClosed 后配置，同时把焦点交给输入框）
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

	// 窗口聚焦：标记状态、清零当前会话未读、把焦点交给输入框
	a.fyneApp.Lifecycle().SetOnEnteredForeground(func() {
		a.windowFocused = true
		a.mu.Lock()
		sid := a.currentSID
		a.mu.Unlock()
		a.clearUnread(sid)
		if a.chatInput != nil {
			a.window.Canvas().Focus(a.chatInput)
		}
	})

	// 先 Show 再 Run，确保 canvas 初始化后立刻把焦点给输入框，
	// 避免某些平台 OnEnteredForeground 不会在首次显示时触发导致光标不出现。
	a.window.Show()
	if a.chatInput != nil {
		a.window.Canvas().Focus(a.chatInput)
		// 事件循环跑起来后某些平台会把焦点抢回 List / 窗口本身，
		// 这里再补一次 Focus 兜底让光标确实出现。
		time.AfterFunc(300*time.Millisecond, func() {
			if a.chatInput != nil {
				a.window.Canvas().Focus(a.chatInput)
			}
		})
	}
	a.fyneApp.Run()
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
			icon := widget.NewIcon(theme.AccountIcon())
			name := widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
			// 副标题（IP / 离线 / N 人）用主前景色，颜色比默认 Disabled 灰加深更清晰。
			sub := canvas.NewText("", theme.Color(theme.ColorNameForeground))
			sub.TextSize = theme.TextSize() - 2
			info := container.NewVBox(name, sub)
			// 未读红点徽标：圆角矩形 + 白色数字
			badgeBg := canvas.NewRectangle(color.NRGBA{R: 227, G: 60, B: 60, A: 255})
			badgeBg.CornerRadius = 9
			badgeText := canvas.NewText("", color.White)
			badgeText.Alignment = fyne.TextAlignCenter
			badgeText.TextStyle = fyne.TextStyle{Bold: true}
			badgeText.TextSize = theme.TextSize() - 2
			badge := container.NewGridWrap(fyne.NewSize(28, 18),
				container.NewStack(badgeBg, container.NewCenter(badgeText)))
			badge.Hide()
			return container.NewBorder(nil, nil, icon, badge, info)
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			a.mu.Lock()
			if id >= len(a.filteredItems) {
				a.mu.Unlock()
				return
			}
			item := a.filteredItems[id]
			a.mu.Unlock()

			border := obj.(*fyne.Container)
			// Border: [center, left, right] order
			info := border.Objects[0].(*fyne.Container)
			icon := border.Objects[1].(*widget.Icon)
			badge := border.Objects[2].(*fyne.Container)
			name := info.Objects[0].(*widget.Label)
			sub := info.Objects[1].(*canvas.Text)
			// Badge 内部：Stack(bg, Center(text))
			badgeInner := badge.Objects[0].(*fyne.Container)
			badgeText := badgeInner.Objects[1].(*fyne.Container).Objects[0].(*canvas.Text)

			if item.isSep {
				icon.Hide()
				badge.Hide()
				sub.Hide()
				name.TextStyle = fyne.TextStyle{Italic: true}
				name.SetText(item.name)
				return
			}

			icon.Show()
			name.TextStyle = fyne.TextStyle{Bold: true}

			if item.isGroup {
				icon.SetResource(theme.MailComposeIcon())
			} else if item.online {
				icon.SetResource(theme.AccountIcon())
			} else {
				icon.SetResource(theme.VisibilityOffIcon())
			}
			name.SetText(item.name)

			if item.subtitle == "" {
				sub.Hide()
			} else {
				sub.Show()
				sub.Text = item.subtitle
				sub.Color = theme.Color(theme.ColorNameForeground)
				sub.Refresh()
			}

			if item.unread > 0 {
				if item.unread > 99 {
					badgeText.Text = "99+"
				} else {
					badgeText.Text = fmt.Sprintf("%d", item.unread)
				}
				badgeText.Refresh()
				badge.Show()
			} else {
				badge.Hide()
			}
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

	a.selfInfoLabel = widget.NewLabel("")
	a.selfInfoLabel.TextStyle = fyne.TextStyle{Italic: true}
	a.refreshSelfInfo()

	bottomBar := container.NewVBox(
		container.NewPadded(a.selfInfoLabel),
		createGroupBtn,
	)

	leftPanel := container.NewBorder(
		topBar,
		bottomBar,
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
			// 文本用只读 Entry 展示，可鼠标选中并 Ctrl/Cmd+C 复制。
			contentEntry := newReadOnlyEntry()
			// 局部主题：让 Entry 的输入背景透明 + 隐藏滚动条，气泡色透出来。
			contentWrap := container.NewThemeOverride(
				contentEntry,
				&bubbleContentTheme{base: a.fyneApp.Settings().Theme()},
			)

			// 图片气泡：透明 imageTapper 捕获点击，不画悬停灰色 overlay
			img := canvas.NewImageFromResource(nil)
			img.SetMinSize(fyne.NewSize(200, 150))
			img.FillMode = canvas.ImageFillContain
			img.Hidden = true
			imgTap := newImageTapper()
			imgTap.Hide()
			imgContainer := container.NewStack(img, imgTap)
			imgContainer.Hidden = true

			// 文件卡片：图标 + 文件名 + 底部（meta + 动态按钮/状态区）
			fileIcon := widget.NewIcon(theme.FileIcon())
			fileName := widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
			fileMeta := canvas.NewText("", theme.Color(theme.ColorNameDisabled))
			fileMeta.TextSize = theme.TextSize() - 2
			// 五类按钮/状态，按 msg.Type 动态 Show/Hide
			fileOpenBtn := widget.NewButton("打开", nil)
			fileOpenBtn.Importance = widget.LowImportance
			fileOpenBtn.Hide()
			fileSaveBtn := widget.NewButton("另存", nil)
			fileSaveBtn.Importance = widget.LowImportance
			fileSaveBtn.Hide()
			fileAcceptBtn := widget.NewButton("接收", nil)
			fileAcceptBtn.Importance = widget.HighImportance
			fileAcceptBtn.Hide()
			fileRejectBtn := widget.NewButton("拒绝", nil)
			fileRejectBtn.Importance = widget.LowImportance
			fileRejectBtn.Hide()
			fileStatusLabel := widget.NewLabel("")
			fileStatusLabel.Hide()
			fileAction := container.NewHBox(fileStatusLabel, fileRejectBtn, fileAcceptBtn, fileSaveBtn, fileOpenBtn)
			fileBottom := container.NewBorder(nil, nil, fileMeta, fileAction, widget.NewLabel(""))
			fileRight := container.NewVBox(fileName, fileBottom)
			fileCard := container.NewBorder(nil, nil, fileIcon, nil, fileRight)
			fileCard.Hidden = true

			// 气泡背景 + 内容
			bubbleRect := canvas.NewRectangle(color.Transparent)
			bubbleRect.CornerRadius = 10
			// 气泡内只放三种内容容器（同一时间只有一个可见），tightBubbleLayout 把可见那个铺满。
			// 气泡外的时间标签由 bubbleRowLayout 统一放置。
			bubbleContent := container.New(&tightBubbleLayout{}, contentWrap, imgContainer, fileCard)
			innerBox := container.NewPadded(bubbleContent)
			bubble := container.NewStack(bubbleRect, innerBox)
			timeLabel := canvas.NewText("", theme.Color(theme.ColorNameDisabled))
			timeLabel.TextSize = theme.TextSize() - 2
			// 每个 row 绑定自己的 bubbleRowLayout 实例，update 时切换 rightAlign
			return container.New(&bubbleRowLayout{}, bubble, timeLabel)
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
			localPort := a.discovery.TCPPort()
			a.mu.Unlock()

			row := obj.(*fyne.Container)
			rowLayout := row.Layout.(*bubbleRowLayout)
			bubble := row.Objects[0].(*fyne.Container)
			timeLabel := row.Objects[1].(*canvas.Text)

			bubbleRect := bubble.Objects[0].(*canvas.Rectangle)
			paddedBox := bubble.Objects[1].(*fyne.Container)
			vbox := paddedBox.Objects[0].(*fyne.Container)
			contentWrap := vbox.Objects[0].(*container.ThemeOverride) // 包裹只读 Entry
			contentEntry := contentWrap.Content.(*readOnlyEntry)
			imgContainer := vbox.Objects[1].(*fyne.Container)
			fileCard := vbox.Objects[2].(*fyne.Container)
			img := imgContainer.Objects[0].(*canvas.Image)
			imgTap := imgContainer.Objects[1].(*imageTapper)
			fileRight := fileCard.Objects[0].(*fyne.Container)
			fileName := fileRight.Objects[0].(*widget.Label)
			fileBottom := fileRight.Objects[1].(*fyne.Container)
			// fileBottom 是 Border：Objects = [center, left(meta), right(action)]
			fileMeta := fileBottom.Objects[1].(*canvas.Text)
			fileAction := fileBottom.Objects[2].(*fyne.Container)
			// fileAction HBox: [status, reject, accept, save, open]
			fileStatusLabel := fileAction.Objects[0].(*widget.Label)
			fileRejectBtn := fileAction.Objects[1].(*widget.Button)
			fileAcceptBtn := fileAction.Objects[2].(*widget.Button)
			fileSaveBtn := fileAction.Objects[3].(*widget.Button)
			fileOpenBtn := fileAction.Objects[4].(*widget.Button)

			// 气泡方向：同机多实例需按 IP+Port 判断
			isMine := msg.FromIP == localIP && msg.FromPort == localPort
			rowLayout.rightAlign = isMine
			variant := a.fyneApp.Settings().ThemeVariant()
			if isMine {
				bubbleRect.FillColor = selfBubbleColor(variant)
			} else {
				bubbleRect.FillColor = otherBubbleColor(variant)
			}
			bubbleRect.Refresh()

			timeLabel.Text = time.Unix(msg.Timestamp, 0).Format("15:04")
			timeLabel.Color = theme.Color(theme.ColorNameDisabled)
			timeLabel.Refresh()

			switch msg.Type {
			case model.ChatImage:
				contentWrap.Hide()
				imgContainer.Show()
				img.Show()
				imgTap.Show()
				fileCard.Hide()
				if msg.Content != "" {
					img.File = msg.Content
					img.Refresh()
				}
				imgPath := msg.Content
				imgTap.onTapped = func() { a.showFullImage(imgPath) }
			case model.ChatFile, model.ChatFileRequest, model.ChatFileRejected, model.ChatFileFailed:
				contentWrap.Hide()
				imgContainer.Hide()
				fileCard.Show()
				fileName.SetText(msg.Filename)
				a.setupFileCard(msg, isMine, fileMeta, fileOpenBtn, fileSaveBtn, fileAcceptBtn, fileRejectBtn, fileStatusLabel)
			default:
				contentWrap.Show()
				contentEntry.SetText(msg.Content)
				imgContainer.Hide()
				fileCard.Hide()
			}

			// 计算气泡目标尺寸，避免 WrapWord 下 MinSize 只返 1 行高导致溢出
			targetW, targetH := a.measureBubble(msg)
			rowLayout.targetW = targetW
			rowLayout.targetH = targetH
			row.Refresh()
			if targetH > 0 {
				a.chatHistory.SetItemHeight(id, targetH)
			}
		},
	)
	// 消息行不保留选中态：一被 List 选中立即取消，避免出现蓝色高亮背景。
	a.chatHistory.OnSelected = func(id widget.ListItemID) {
		a.chatHistory.Unselect(id)
	}

	// 自定义聊天输入框
	a.chatInput = newChatEntry(func() { a.sendTextMessage() })
	a.chatInput.onPasteImage = a.tryPasteClipboardImage
	a.chatInput.SetMinRowsVisible(3)

	// 加载发送模式设置
	sendMode, _ := a.store.GetSetting("send_mode")
	a.chatInput.enterToSend = sendMode != "ctrl_enter"

	sendBtn := widget.NewButtonWithIcon("发送", theme.MailSendIcon(), func() {
		a.sendTextMessage()
	})

	// 工具栏：小图标按钮
	fileBtn := widget.NewButtonWithIcon("", theme.FolderOpenIcon(), func() { a.sendFileRequest() })
	imgBtn := widget.NewButtonWithIcon("", theme.FileImageIcon(), func() { a.sendImageMessage() })
	emojiBtn := widget.NewButton("😀", func() { a.showEmojiPicker() })
	emojiBtn.Importance = widget.LowImportance
	screenshotBtn := widget.NewButtonWithIcon("", theme.ContentCutIcon(), func() { a.startScreenshot() })

	// 工具栏加一层背景矩形，做成"聊天区与输入框之间的条带"
	toolBg := canvas.NewRectangle(toolbarBgColor(a.fyneApp.Settings().ThemeVariant()))
	toolRow := container.NewStack(
		toolBg,
		container.NewHBox(fileBtn, imgBtn, emojiBtn, screenshotBtn),
	)
	// 用单独的主题覆盖包住 chatInput，把光标（primary）强制改成不可能看错的亮红，
	// 以确认光标是否被绘制出来；同时背景保持纯白，避免任何对比度问题。
	chatInputWrap := container.NewThemeOverride(a.chatInput,
		&chatInputTheme{base: a.fyneApp.Settings().Theme()})
	inputRow := container.NewBorder(nil, nil, nil, sendBtn, chatInputWrap)
	// 工具栏与上方聊天区之间留 20px 空隙，避免最后一条气泡贴着工具条。
	toolbarGap := canvas.NewRectangle(color.Transparent)
	toolbarGap.SetMinSize(fyne.NewSize(1, 20))
	inputBar := container.NewVBox(toolbarGap, toolRow, inputRow)

	chatTitle := widget.NewLabel("选择一个用户或群聊开始聊天")
	a.chatTitleLabel = chatTitle

	searchHistoryBtn := widget.NewButtonWithIcon("", theme.SearchIcon(), func() {
		a.showSearchDialog()
	})
	chatTitleBar := container.NewHBox(chatTitle, layout.NewSpacer(), searchHistoryBtn)

	// List 行的 hover/selection/focus 颜色透明化已在 appTheme 里完成
	// （listItem 包装层不在 ThemeOverride 的 scope 中，必须在 app 级改）。
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
			FromPort:  msg.FromPort,
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
		// 非当前会话或窗口不聚焦 → 计入未读
		if !isCurrentSession || !a.windowFocused {
			a.unreadCounts[sessionID]++
		}
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

	a.mu.Lock()
	unread := make(map[string]int, len(a.unreadCounts))
	for k, v := range a.unreadCounts {
		unread[k] = v
	}
	a.mu.Unlock()

	items := make([]sideItem, 0, len(users)+len(groups)+1)
	for _, u := range users {
		sub := u.IP
		if !u.Online {
			sub = "离线"
		}
		items = append(items, sideItem{
			label:    u.Nickname + " " + u.IP,
			name:     u.Nickname,
			subtitle: sub,
			id:       u.Key(),
			scope:    model.ScopePrivate,
			online:   u.Online,
			isGroup:  false,
			unread:   unread[u.Key()],
		})
	}
	if len(groups) > 0 {
		items = append(items, sideItem{
			name:  "── 群聊 ──",
			label: "── 群聊 ──",
			id:    "_sep",
			scope: "_sep",
			isSep: true,
		})
		for _, g := range groups {
			items = append(items, sideItem{
				label:    g.Name,
				name:     g.Name,
				subtitle: fmt.Sprintf("%d 人", len(g.Members)),
				id:       g.ID,
				scope:    model.ScopeGroup,
				isGroup:  true,
				unread:   unread[g.ID],
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
	// 清除未读
	clearedUnread := a.unreadCounts[id] > 0
	delete(a.unreadCounts, id)
	a.mu.Unlock()

	a.chatTitleLabel.SetText(fmt.Sprintf("与 %s 的对话", label))
	a.chatHistory.Refresh()
	if len(s.messages) > 0 {
		a.chatHistory.ScrollToBottom()
	}
	if clearedUnread {
		a.scheduleRefreshSidePanel()
	}
	// 切换会话后把焦点交给输入框，方便直接打字
	if a.chatInput != nil {
		a.window.Canvas().Focus(a.chatInput)
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
		FromPort:  a.discovery.TCPPort(),
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
	a.clearUnread(s.id)
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
			FromPort:  a.discovery.TCPPort(),
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
		a.clearUnread(s.id)
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

// sendFileRequest 发起文件请求：选择文件后在自己聊天里显示一条"等待对方接受…"
// 的卡片，同时通过 TCP 发送 ChatFileRequest 给对方；5 分钟未收到接受就标为传输失败。
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
		now := time.Now().Unix()

		// 1) 本地存一条 ChatFileRequest：Content 放 fileID，Filename 放文件名
		stored := &model.StoredMessage{
			SessionID: s.id,
			Scope:     s.scope,
			FromNick:  a.discovery.Nickname(),
			FromIP:    a.discovery.LocalIP(),
			FromPort:  a.discovery.TCPPort(),
			Type:      model.ChatFileRequest,
			Content:   fileID,
			Filename:  filename,
			Timestamp: now,
		}
		if err := a.store.SaveMessage(stored); err != nil {
			dialog.ShowError(fmt.Errorf("保存文件记录失败: %v", err), a.window)
			return
		}

		a.mu.Lock()
		s.messages = append(s.messages, stored)
		a.mu.Unlock()

		// 2) 登记请求状态（发送方）
		state := &fileRequestState{
			fileID:    fileID,
			storedMsg: stored,
			sessionID: s.id,
			localPath: filePath,
			toKey:     s.id, // 单聊 s.id 就是对端 UserKey
			scope:     s.scope,
			groupID:   "",
		}
		if s.scope == model.ScopeGroup {
			state.groupID = s.id
		}
		a.fileMu.Lock()
		a.fileRequests[fileID] = state
		a.fileMu.Unlock()

		// 3) 5 分钟超时
		state.timer = time.AfterFunc(fileRequestTimeout, func() { a.fileRequestTimeout(fileID) })

		// 4) 发出去
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
			a.cancelFileRequest(fileID, model.ChatFileFailed)
			return
		}

		a.clearUnread(s.id)
		a.refreshChatIfCurrent(s.id)
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

// handleFileRequest 接收方：不再弹窗，直接在聊天里显示一条卡片，
// 点击"接收"才开始传输；5 分钟没确认则标为传输失败。
func (a *App) handleFileRequest(msg *model.ChatMessage) {
	var sessionID string
	if msg.Scope == model.ScopeGroup {
		sessionID = msg.GroupID
	} else {
		sessionID = model.UserKey(msg.FromIP, msg.FromPort)
	}

	stored := &model.StoredMessage{
		SessionID: sessionID,
		Scope:     msg.Scope,
		FromNick:  msg.From,
		FromIP:    msg.FromIP,
		FromPort:  msg.FromPort,
		Type:      model.ChatFileRequest,
		Content:   msg.FileID,
		Filename:  msg.Filename,
		Timestamp: msg.Timestamp,
	}
	if err := a.store.SaveMessage(stored); err != nil {
		log.Printf("保存文件请求失败: %v", err)
		return
	}

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
	isCurrent := a.currentSID == sessionID
	if !isCurrent || !a.windowFocused {
		a.unreadCounts[sessionID]++
	}
	a.mu.Unlock()

	state := &fileRequestState{
		fileID:    msg.FileID,
		storedMsg: stored,
		sessionID: sessionID,
		fromIP:    msg.FromIP,
		fromPort:  msg.FromPort,
		fromNick:  msg.From,
		fileSize:  msg.FileSize,
		scope:     msg.Scope,
		groupID:   msg.GroupID,
	}
	a.fileMu.Lock()
	a.fileRequests[msg.FileID] = state
	a.fileMu.Unlock()
	state.timer = time.AfterFunc(fileRequestTimeout, func() { a.fileRequestTimeout(msg.FileID) })

	if isCurrent {
		a.chatHistory.Refresh()
		a.chatHistory.ScrollToBottom()
	}
	a.scheduleRefreshSidePanel()

	if !a.windowFocused || !isCurrent {
		a.fyneApp.SendNotification(fyne.NewNotification(msg.From, "[文件] "+msg.Filename))
	}
}

// acceptIncomingFile 接收方点击"接收"后：登记接收状态、回 ChatFileAccept。
// 消息卡片保持 ChatFileRequest，完成时才会流转到 ChatFile。
func (a *App) acceptIncomingFile(fileID string) {
	a.fileMu.Lock()
	state, ok := a.fileRequests[fileID]
	if !ok {
		a.fileMu.Unlock()
		return
	}
	delete(a.fileRequests, fileID)
	if state.timer != nil {
		state.timer.Stop()
	}

	chunkTotal := int(state.fileSize / fileChunkSize)
	if state.fileSize%fileChunkSize != 0 {
		chunkTotal++
	}
	a.receivingFiles[fileID] = &receivingFile{
		filename:   state.storedMsg.Filename,
		fileSize:   state.fileSize,
		chunkTotal: chunkTotal,
		received:   make(map[int][]byte),
		fromIP:     state.fromIP,
		fromPort:   state.fromPort,
		fromNick:   state.fromNick,
		scope:      state.scope,
		groupID:    state.groupID,
		storedID:   state.storedMsg.ID,
	}
	a.fileMu.Unlock()

	reply := &model.ChatMessage{
		Type:      model.ChatFileAccept,
		Scope:     state.scope,
		GroupID:   state.groupID,
		From:      a.discovery.Nickname(),
		FromIP:    a.discovery.LocalIP(),
		FromPort:  a.discovery.TCPPort(),
		Timestamp: time.Now().Unix(),
		FileID:    fileID,
		Filename:  state.storedMsg.Filename,
	}
	if u := a.discovery.GetUserByKey(model.UserKey(state.fromIP, state.fromPort)); u != nil {
		_ = a.chatSvc.SendMessage(u.IP, u.TCPPort, reply)
	}
	// 接收按钮应该消失；完成时卡片会再刷新成"打开"。
	a.refreshChatIfCurrent(state.sessionID)
}

// rejectIncomingFile 接收方点击"拒绝"：回 ChatFileReject，本地标为已拒绝。
func (a *App) rejectIncomingFile(fileID string) {
	a.fileMu.Lock()
	state, ok := a.fileRequests[fileID]
	if !ok {
		a.fileMu.Unlock()
		return
	}
	delete(a.fileRequests, fileID)
	if state.timer != nil {
		state.timer.Stop()
	}
	a.fileMu.Unlock()

	reply := &model.ChatMessage{
		Type:      model.ChatFileReject,
		Scope:     state.scope,
		GroupID:   state.groupID,
		From:      a.discovery.Nickname(),
		FromIP:    a.discovery.LocalIP(),
		FromPort:  a.discovery.TCPPort(),
		Timestamp: time.Now().Unix(),
		FileID:    fileID,
		Filename:  state.storedMsg.Filename,
	}
	if u := a.discovery.GetUserByKey(model.UserKey(state.fromIP, state.fromPort)); u != nil {
		_ = a.chatSvc.SendMessage(u.IP, u.TCPPort, reply)
	}
	a.updateStoredType(state.storedMsg, model.ChatFileRejected, "")
	a.refreshChatIfCurrent(state.sessionID)
}

// fileRequestTimeout 5 分钟未确认：把消息标为传输失败。
func (a *App) fileRequestTimeout(fileID string) {
	a.cancelFileRequest(fileID, model.ChatFileFailed)
}

// cancelFileRequest 共用清理：从 fileRequests 删除 + 停定时器 + 更新消息状态。
func (a *App) cancelFileRequest(fileID, newType string) {
	a.fileMu.Lock()
	state, ok := a.fileRequests[fileID]
	if !ok {
		a.fileMu.Unlock()
		return
	}
	delete(a.fileRequests, fileID)
	if state.timer != nil {
		state.timer.Stop()
	}
	a.fileMu.Unlock()

	a.updateStoredType(state.storedMsg, newType, "")
	a.refreshChatIfCurrent(state.sessionID)
}

// handleFileAccept 发送方收到接收方 ACCEPT：停止定时器、开始发块。
func (a *App) handleFileAccept(msg *model.ChatMessage) {
	a.fileMu.Lock()
	state, ok := a.fileRequests[msg.FileID]
	if !ok {
		a.fileMu.Unlock()
		return
	}
	delete(a.fileRequests, msg.FileID)
	if state.timer != nil {
		state.timer.Stop()
	}
	a.fileMu.Unlock()

	go a.sendFileChunks(model.UserKey(msg.FromIP, msg.FromPort), msg.FileID,
		state.localPath, state.scope, state.groupID, state.storedMsg)
}

// handleFileReject 发送方收到接收方 REJECT：把自己的消息标为已拒绝。
func (a *App) handleFileReject(msg *model.ChatMessage) {
	a.cancelFileRequest(msg.FileID, model.ChatFileRejected)
}

// sendFileChunks 顺序把文件分块发出去；完成后把发送方的 ChatFileRequest 升级为 ChatFile。
func (a *App) sendFileChunks(targetKey, fileID, filePath, scope, groupID string, stored *model.StoredMessage) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		log.Printf("读取文件失败: %v", err)
		a.updateStoredType(stored, model.ChatFileFailed, "")
		a.refreshChatIfCurrent(stored.SessionID)
		return
	}

	filename := filepath.Base(filePath)
	chunkTotal := len(data) / fileChunkSize
	if len(data)%fileChunkSize != 0 {
		chunkTotal++
	}

	user := a.discovery.GetUserByKey(targetKey)
	if user == nil {
		a.updateStoredType(stored, model.ChatFileFailed, "")
		a.refreshChatIfCurrent(stored.SessionID)
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
			a.updateStoredType(stored, model.ChatFileFailed, "")
			a.refreshChatIfCurrent(stored.SessionID)
			return
		}
	}

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

	// 发送方：把 ChatFileRequest 升级成 ChatFile，Content = 本地原路径
	a.updateStoredType(stored, model.ChatFile, filePath)
	a.refreshChatIfCurrent(stored.SessionID)
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

// handleFileComplete 收到发送方的"传输完成"信令。
// 由于 chat.Service 每条 TCP 消息一个独立 goroutine 入队，N 个 ChatFileData
// 和尾随的 ChatFileComplete 到达 fileEvents 通道的顺序是无法保证的——
// 完全可能 Complete 事件排在某些 Data 事件之前被 handleFileEvents 取出。
// 这里把组装逻辑丢到独立 goroutine 里去等所有分块就位，避免阻塞
// handleFileEvents 继续处理仍在通道里排队的 ChatFileData 事件。
func (a *App) handleFileComplete(msg *model.ChatMessage) {
	go a.finalizeFileReceive(msg.FileID)
}

func (a *App) finalizeFileReceive(fileID string) {
	a.fileMu.Lock()
	rf, ok := a.receivingFiles[fileID]
	a.fileMu.Unlock()
	if !ok {
		return
	}

	// 最长等 30 秒等所有分块就位
	deadline := time.Now().Add(30 * time.Second)
	for {
		a.fileMu.Lock()
		done := len(rf.received) >= rf.chunkTotal
		a.fileMu.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	a.fileMu.Lock()
	delete(a.receivingFiles, fileID)
	var fileData []byte
	missing := -1
	for i := 0; i < rf.chunkTotal; i++ {
		chunk, exists := rf.received[i]
		if !exists {
			missing = i
			break
		}
		fileData = append(fileData, chunk...)
	}
	a.fileMu.Unlock()

	sessionID := model.UserKey(rf.fromIP, rf.fromPort)
	if rf.scope == model.ScopeGroup {
		sessionID = rf.groupID
	}

	if missing >= 0 {
		log.Printf("文件块 %d 缺失（等待 30s 后仍未到齐）", missing)
		a.updateStoredTypeByID(rf.storedID, model.ChatFileFailed, "")
		a.refreshChatIfCurrent(sessionID)
		return
	}

	destPath := filepath.Join(a.store.FileDir(), fmt.Sprintf("%d_%s", time.Now().Unix(), rf.filename))
	if err := os.WriteFile(destPath, fileData, 0644); err != nil {
		log.Printf("写入文件失败: %v", err)
		a.updateStoredTypeByID(rf.storedID, model.ChatFileFailed, "")
		a.refreshChatIfCurrent(sessionID)
		return
	}

	// 接收方的 ChatFileRequest 消息流转为 ChatFile，Content = 本地路径
	a.updateStoredTypeByID(rf.storedID, model.ChatFile, destPath)
	a.refreshChatIfCurrent(sessionID)
	a.scheduleRefreshSidePanel()
}

// updateStoredType 就地更新 StoredMessage 的 Type/Content，并写回 DB。
func (a *App) updateStoredType(stored *model.StoredMessage, newType, newContent string) {
	a.mu.Lock()
	stored.Type = newType
	stored.Content = newContent
	a.mu.Unlock()
	if err := a.store.UpdateMessageTypeContent(stored.ID, newType, newContent); err != nil {
		log.Printf("更新消息状态失败: %v", err)
	}
}

// updateStoredTypeByID 通过 ID 找到会话里的消息并更新；主要给接收方完成/失败时用。
func (a *App) updateStoredTypeByID(id int64, newType, newContent string) {
	a.mu.Lock()
	for _, s := range a.sessions {
		for _, m := range s.messages {
			if m.ID == id {
				m.Type = newType
				m.Content = newContent
				break
			}
		}
	}
	a.mu.Unlock()
	if err := a.store.UpdateMessageTypeContent(id, newType, newContent); err != nil {
		log.Printf("更新消息状态失败: %v", err)
	}
}

// refreshChatIfCurrent 仅当目标会话就是当前选中会话时，刷新聊天区。
func (a *App) refreshChatIfCurrent(sessionID string) {
	a.mu.Lock()
	isCurrent := a.currentSID == sessionID
	a.mu.Unlock()
	if isCurrent {
		a.chatHistory.Refresh()
	}
}

// setupFileCard 根据消息 Type 切换文件卡片的按钮/状态区显示。
func (a *App) setupFileCard(msg *model.StoredMessage, isMine bool,
	fileMeta *canvas.Text,
	fileOpenBtn, fileSaveBtn, fileAcceptBtn, fileRejectBtn *widget.Button,
	fileStatusLabel *widget.Label) {
	// 默认全部隐藏，再按 Type 打开需要的
	fileOpenBtn.Hide()
	fileSaveBtn.Hide()
	fileAcceptBtn.Hide()
	fileRejectBtn.Hide()
	fileStatusLabel.Hide()

	fileMeta.Color = theme.Color(theme.ColorNameDisabled)

	switch msg.Type {
	case model.ChatFile:
		meta := "文件"
		if fi, err := os.Stat(msg.Content); err == nil {
			meta = formatFileSize(fi.Size())
		}
		fileMeta.Text = meta
		fileMeta.Refresh()
		fileOpenBtn.Show()
		localPath := msg.Content
		filename := msg.Filename
		fileOpenBtn.OnTapped = func() { openPath(localPath) }
		// 另存仅对接收方显示：发送方 localPath 就是原文件，复制无意义。
		if !isMine {
			fileSaveBtn.Show()
			fileSaveBtn.OnTapped = func() { a.showSaveFileDialog(localPath, filename) }
		}
	case model.ChatFileRequest:
		fileMeta.Text = "文件"
		fileMeta.Refresh()
		fileID := msg.Content
		// ChatFileRequest 只是初始态，一旦 accept/reject 后实际的传输状态
		// 由内存中的 fileRequests / receivingFiles 决定。
		a.fileMu.Lock()
		_, stillPending := a.fileRequests[fileID]
		_, receiving := a.receivingFiles[fileID]
		a.fileMu.Unlock()
		if isMine {
			switch {
			case stillPending:
				fileStatusLabel.SetText("等待对方接受…")
			default:
				// 不在 fileRequests 里了：对方已 accept，正在发送分块
				fileStatusLabel.SetText("发送中…")
			}
			fileStatusLabel.Show()
		} else {
			switch {
			case stillPending:
				fileAcceptBtn.Show()
				fileRejectBtn.Show()
				fileAcceptBtn.OnTapped = func() { a.acceptIncomingFile(fileID) }
				fileRejectBtn.OnTapped = func() { a.rejectIncomingFile(fileID) }
			case receiving:
				fileStatusLabel.SetText("接收中…")
				fileStatusLabel.Show()
			default:
				// 极少数情况：请求已消失但又没进入 receivingFiles
				fileStatusLabel.SetText("接收中…")
				fileStatusLabel.Show()
			}
		}
	case model.ChatFileRejected:
		fileMeta.Text = ""
		fileMeta.Refresh()
		fileStatusLabel.SetText("已拒绝")
		fileStatusLabel.Show()
	case model.ChatFileFailed:
		fileMeta.Text = ""
		fileMeta.Refresh()
		fileStatusLabel.SetText("传输失败")
		fileStatusLabel.Show()
	}
}

// appTheme 全局主题：在用户选定的基础主题上做若干定制。
// - 输入框描边为 0、背景纯白，让光标清晰可见；
// - primary 色改为浅绿（widget.Entry 的光标走 primary），用于排查光标不可见问题；
// - 分隔线厚度为 0，隐藏 widget.List 条目间灰色横线。
type appTheme struct {
	base fyne.Theme
}

var (
	inputBgWhite    = color.RGBA{R: 255, G: 255, B: 255, A: 255}
	cursorLightGrn  = color.RGBA{R: 76, G: 175, B: 80, A: 255}  // #4CAF50 偏亮绿，对比纯白
)

func (t *appTheme) Color(n fyne.ThemeColorName, v fyne.ThemeVariant) color.Color {
	switch n {
	case theme.ColorNameInputBackground:
		return inputBgWhite
	case theme.ColorNamePrimary:
		return cursorLightGrn
	case theme.ColorNameHover, theme.ColorNameSelection, theme.ColorNameFocus,
		theme.ColorNamePressed:
		// widget.List 的 listItem 包装层不在 ThemeOverride 的 scope 里——
		// 它拿到的是 theme.Current() 即本 appTheme。所以要在这里把悬停/选中
		// 的底色直接吃成透明，才能真正消掉消息列表行上的灰色覆盖。
		// （会顺带影响用户列表：用户没投诉那里有悬停灰，视觉更干净。）
		return color.Transparent
	}
	return t.base.Color(n, v)
}
func (t *appTheme) Font(s fyne.TextStyle) fyne.Resource { return t.base.Font(s) }
func (t *appTheme) Icon(n fyne.ThemeIconName) fyne.Resource {
	return t.base.Icon(n)
}
func (t *appTheme) Size(n fyne.ThemeSizeName) float32 {
	switch n {
	case theme.SizeNameInputBorder:
		return 0
	case theme.SizeNameSeparatorThickness:
		return 0
	}
	return t.base.Size(n)
}

// chatInputTheme 专门给底部输入框用：把 primary 强制改成亮红，
// 这样 Fyne Entry 用 PrimaryColor 画出来的光标就一定能肉眼看到。
// 除此之外保持基础主题，避免影响全局按钮/控件的高亮色。
type chatInputTheme struct {
	base fyne.Theme
}

var cursorBrightRed = color.NRGBA{R: 255, G: 40, B: 40, A: 255}

func (t *chatInputTheme) Color(n fyne.ThemeColorName, v fyne.ThemeVariant) color.Color {
	switch n {
	case theme.ColorNamePrimary, theme.ColorNameFocus:
		return cursorBrightRed
	case theme.ColorNameInputBackground:
		return inputBgWhite
	}
	return t.base.Color(n, v)
}
func (t *chatInputTheme) Font(s fyne.TextStyle) fyne.Resource     { return t.base.Font(s) }
func (t *chatInputTheme) Icon(n fyne.ThemeIconName) fyne.Resource { return t.base.Icon(n) }
func (t *chatInputTheme) Size(n fyne.ThemeSizeName) float32 {
	return t.base.Size(n)
}

// bubbleContentTheme 用于气泡内文本 Entry 的局部主题：
//   - InputBackground 透明，让气泡底色透出来
//   - 滚动条透明 + 尺寸为 0，隐藏 Entry 在 Wrap 模式下一定会创建的滚动条
//
// 其它走外层 appTheme。
type bubbleContentTheme struct {
	base fyne.Theme
}

func (t *bubbleContentTheme) Color(n fyne.ThemeColorName, v fyne.ThemeVariant) color.Color {
	switch n {
	case theme.ColorNameInputBackground:
		return color.Transparent
	case theme.ColorNameScrollBar, theme.ColorNameScrollBarBackground:
		return color.Transparent
	case theme.ColorNameForeground:
		// 气泡内正文颜色加深：浅色主题下几乎纯黑，深色主题下用偏亮的浅灰，
		// 避免默认前景色在浅色气泡里太淡。
		if v == theme.VariantDark {
			return color.NRGBA{R: 235, G: 235, B: 235, A: 255}
		}
		return color.NRGBA{R: 20, G: 20, B: 20, A: 255}
	}
	return t.base.Color(n, v)
}
func (t *bubbleContentTheme) Font(s fyne.TextStyle) fyne.Resource    { return t.base.Font(s) }
func (t *bubbleContentTheme) Icon(n fyne.ThemeIconName) fyne.Resource { return t.base.Icon(n) }
func (t *bubbleContentTheme) Size(n fyne.ThemeSizeName) float32 {
	switch n {
	case theme.SizeNameScrollBar, theme.SizeNameScrollBarSmall:
		return 0
	}
	return t.base.Size(n)
}

// wrapBaseTheme 从设置字符串解析出基础主题并包一层 appTheme。
func wrapBaseTheme(setting string) fyne.Theme {
	var base fyne.Theme
	switch setting {
	case "light":
		base = theme.LightTheme()
	case "dark":
		base = theme.DarkTheme()
	default:
		base = theme.DefaultTheme()
	}
	return &appTheme{base: base}
}

// bubbleRowLayout 把气泡 + 时间标签按左/右对齐放在整行里。
// objs[0] = bubble（必需），objs[1] = timeLabel（可选）。
// 时间标签挂在气泡外侧、底部对齐：
//   - 发送方（rightAlign=true）气泡靠右，时间贴在气泡左侧底部；
//   - 接收方气泡靠左，时间贴在气泡右侧底部。
//
// update 回调会把 targetW / targetH 填好（通过 fyne.MeasureText 手算），
// MinSize/Layout 就按目标尺寸走——避免了 WrapWord Entry MinSize
// 只返 1 行高导致内容溢出的问题。
type bubbleRowLayout struct {
	rightAlign bool
	targetW    float32 // <=0 表示按气泡 MinSize
	targetH    float32 // <=0 表示按气泡 MinSize
}

const (
	bubbleMaxRatio = 0.72
	bubbleMinWidth = 80
	// messageRowGap 是相邻两条消息之间的垂直间距（px）。行的总高度
	// = 气泡高度 + messageRowGap，gap 总是放在气泡下方。
	messageRowGap = 10
)

func (l *bubbleRowLayout) MinSize(objs []fyne.CanvasObject) fyne.Size {
	if len(objs) == 0 {
		return fyne.NewSize(0, 0)
	}
	h := l.targetH
	if h <= 0 {
		h = objs[0].MinSize().Height
	}
	return fyne.NewSize(0, h)
}

func (l *bubbleRowLayout) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	if len(objs) == 0 {
		return
	}
	bubble := objs[0]
	var timeLabel fyne.CanvasObject
	if len(objs) > 1 {
		timeLabel = objs[1]
	}

	w := l.targetW
	if w <= 0 {
		w = bubble.MinSize().Width
	}
	maxW := size.Width * bubbleMaxRatio
	if w > maxW {
		w = maxW
	}
	if w < bubbleMinWidth && bubbleMinWidth <= size.Width {
		w = bubbleMinWidth
	}
	if w > size.Width {
		w = size.Width
	}
	// 气泡高度 = 行高 - 间距；行尾部那段空白就是和下一条消息的 gap。
	h := l.targetH - messageRowGap
	if h <= 0 {
		h = size.Height - messageRowGap
		if h < 0 {
			h = 0
		}
	}

	gap := theme.Padding()
	var timeW, timeH float32
	if timeLabel != nil {
		ms := timeLabel.MinSize()
		timeW, timeH = ms.Width, ms.Height
	}

	if l.rightAlign {
		bubbleX := size.Width - w
		bubble.Move(fyne.NewPos(bubbleX, 0))
		bubble.Resize(fyne.NewSize(w, h))
		if timeLabel != nil {
			tx := bubbleX - gap - timeW
			if tx < 0 {
				tx = 0
			}
			ty := h - timeH
			if ty < 0 {
				ty = 0
			}
			timeLabel.Move(fyne.NewPos(tx, ty))
			timeLabel.Resize(fyne.NewSize(timeW, timeH))
		}
	} else {
		bubble.Move(fyne.NewPos(0, 0))
		bubble.Resize(fyne.NewSize(w, h))
		if timeLabel != nil {
			tx := w + gap
			ty := h - timeH
			if ty < 0 {
				ty = 0
			}
			timeLabel.Move(fyne.NewPos(tx, ty))
			timeLabel.Resize(fyne.NewSize(timeW, timeH))
		}
	}
}

// tightBubbleLayout 紧凑气泡内容布局：气泡内同一时间只可见一个对象
// （文本 Entry / 图片容器 / 文件卡片），把它铺满整个气泡内部。
//
// 与 container.NewStack 的区别：这里只会为可见对象返回 MinSize，
// 避免被隐藏的文件卡片抬高气泡高度。
type tightBubbleLayout struct{}

func (l *tightBubbleLayout) MinSize(objs []fyne.CanvasObject) fyne.Size {
	for _, o := range objs {
		if !o.Visible() {
			continue
		}
		return o.MinSize()
	}
	return fyne.NewSize(0, 0)
}

func (l *tightBubbleLayout) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	for _, o := range objs {
		if !o.Visible() {
			continue
		}
		o.Move(fyne.NewPos(0, 0))
		o.Resize(size)
		return
	}
}

// measureBubble 估算单条消息气泡的目标宽高。
// 返回 (w, h)，任一为 0 时表示该维度回退到气泡自身 MinSize。
//
// 气泡层级：Stack(矩形) → Padded(pad 四周) → tightBubbleLayout(单一内容)。
// 横向内边距 = pad*2，纵向内边距 = pad*2。
func (a *App) measureBubble(msg *model.StoredMessage) (float32, float32) {
	textSize := theme.TextSize()
	pad := theme.Padding()
	innerPad := theme.InnerPadding()
	// 文本气泡横向 = Padded(pad*2) + Entry 内部 InnerPadding*2；
	// 非文本气泡只有 Padded(pad*2)。
	textHPad := pad*2 + innerPad*2
	nonTextHPad := pad * 2
	innerPadV := pad * 2 // Padded 上下

	rowW := a.chatHistory.Size().Width
	if rowW < 100 {
		if cs := a.window.Canvas().Size(); cs.Width > 0 {
			rowW = cs.Width * 0.72
		} else {
			rowW = 640
		}
	}
	maxBubbleW := rowW * bubbleMaxRatio
	maxContentW := maxBubbleW - textHPad
	if maxContentW < 80 {
		maxContentW = 80
	}

	lineH := fyne.MeasureText("国", textSize, fyne.TextStyle{}).Height
	// widget.Entry 多行渲染每行实际占用 > MeasureText 返回的紧排高度：
	// Fyne 内部有约 1.2 的 line-height 因子。pad*2 已覆盖。
	effLineH := lineH + pad*2

	switch msg.Type {
	case model.ChatImage:
		// 图片最小 200x150
		w := float32(200) + nonTextHPad
		if w > maxBubbleW {
			w = maxBubbleW
		}
		h := innerPadV + 150
		return w, h + messageRowGap
	case model.ChatFile, model.ChatFileRequest, model.ChatFileRejected, model.ChatFileFailed:
		// 文件卡片：固定偏宽（文件名 + 状态/按钮区）
		w := maxBubbleW * 0.65
		if w < 280 {
			w = 280
		}
		if w > maxBubbleW {
			w = maxBubbleW
		}
		// 文件卡片内含图标 + VBox(文件名行, 底部 meta+按钮行)。
		// Fyne Button 自带 InnerPadding 两侧共 16px，实测按钮行 ~34px；
		// 文件名行 ~22px；VBox 之间再加 pad。外层气泡再留 innerPadV。
		// 底部额外多留 pad*2 裕量，避免按钮贴下边。
		nameRowH := lineH + pad*2
		btnRowH := lineH + innerPad*2 + pad*2
		h := innerPadV + nameRowH + pad + btnRowH + pad*2
		return w, h + messageRowGap
	default:
		// 文本：按行测量，超过 maxContentW 则模拟换行（ceil）
		lines := 0
		maxLineW := float32(0)
		for _, line := range strings.Split(msg.Content, "\n") {
			if line == "" {
				lines++
				continue
			}
			lineW := fyne.MeasureText(line, textSize, fyne.TextStyle{}).Width
			if lineW <= maxContentW {
				lines++
				if lineW > maxLineW {
					maxLineW = lineW
				}
			} else {
				// ceil(lineW / maxContentW)
				n := int(lineW / maxContentW)
				if lineW > float32(n)*maxContentW {
					n++
				}
				lines += n
				maxLineW = maxContentW
			}
		}
		if lines == 0 {
			lines = 1
		}
		// 宽度 = 最长行宽 + 全部水平 padding（含 Entry InnerPadding），
		// 刚够装下最长行就不会再在 Entry 内被换行。
		w := maxLineW + textHPad
		if w > maxBubbleW {
			w = maxBubbleW
		}
		// 高度上下对称：Padded 上下各 pad，Entry 上下各 InnerPadding。
		// 不再额外加 pad*2 裕量，避免底部空出一截。
		entryOverhead := innerPad * 2
		h := innerPadV + float32(lines)*effLineH + entryOverhead
		return w, h + messageRowGap
	}
}

// refreshSelfInfo 更新左侧底部当前用户的昵称/地址显示。
func (a *App) refreshSelfInfo() {
	if a.selfInfoLabel == nil {
		return
	}
	a.selfInfoLabel.SetText(fmt.Sprintf("我: %s (%s:%d)",
		a.discovery.Nickname(), a.discovery.LocalIP(), a.discovery.TCPPort()))
}

// clearUnread 清除指定会话的未读计数（如果有）并刷新侧边栏。
func (a *App) clearUnread(sessionID string) {
	if sessionID == "" {
		return
	}
	a.mu.Lock()
	had := a.unreadCounts[sessionID] > 0
	delete(a.unreadCounts, sessionID)
	a.mu.Unlock()
	if had {
		a.scheduleRefreshSidePanel()
	}
}

// selfBubbleColor / otherBubbleColor 返回不同主题下的气泡背景色。
// 选择浅色使默认文本颜色（暗/亮主题自适应）依然清晰可读。
func selfBubbleColor(variant fyne.ThemeVariant) color.Color {
	if variant == theme.VariantDark {
		return color.NRGBA{R: 46, G: 92, B: 60, A: 255} // 深绿
	}
	return color.NRGBA{R: 197, G: 234, B: 166, A: 255} // 浅绿（仿微信）
}

func otherBubbleColor(variant fyne.ThemeVariant) color.Color {
	if variant == theme.VariantDark {
		return color.NRGBA{R: 62, G: 62, B: 64, A: 255} // 深灰
	}
	return color.NRGBA{R: 244, G: 244, B: 246, A: 255} // 近白
}

// toolbarBgColor 输入框上方工具栏条带的底色，浅色模式用淡灰，深色模式用偏暗灰。
func toolbarBgColor(variant fyne.ThemeVariant) color.Color {
	if variant == theme.VariantDark {
		return color.NRGBA{R: 52, G: 52, B: 55, A: 255}
	}
	return color.NRGBA{R: 238, G: 238, B: 242, A: 255}
}

// showSaveFileDialog 弹出"另存为"对话框，把 srcPath 指向的文件复制到用户选定的位置。
func (a *App) showSaveFileDialog(srcPath, filename string) {
	dlg := dialog.NewFileSave(func(writer fyne.URIWriteCloser, err error) {
		if err != nil {
			dialog.ShowError(err, a.window)
			return
		}
		if writer == nil {
			return
		}
		defer writer.Close()
		data, err := os.ReadFile(srcPath)
		if err != nil {
			dialog.ShowError(fmt.Errorf("读取源文件失败: %v", err), a.window)
			return
		}
		if _, err := writer.Write(data); err != nil {
			dialog.ShowError(fmt.Errorf("写入文件失败: %v", err), a.window)
			return
		}
	}, a.window)
	dlg.SetFileName(filename)
	dlg.Show()
}

// openPath 用系统默认程序打开文件或目录
func openPath(path string) {
	if path == "" {
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("打开 %s 失败: %v", path, err)
	}
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
		FromPort:  a.discovery.TCPPort(),
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
	a.clearUnread(s.id)
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
			a.refreshSelfInfo()
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
		case "暗色":
			themeVal = "dark"
		default:
			themeVal = ""
		}
		a.fyneApp.Settings().SetTheme(wrapBaseTheme(themeVal))
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

