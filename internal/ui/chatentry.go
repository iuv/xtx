package ui

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/widget"
)

// chatEntry 自定义聊天输入框，支持回车发送和剪贴板图片粘贴
type chatEntry struct {
	widget.Entry
	onSend       func()
	onPasteImage func() bool // 返回 true 表示已处理图片粘贴
	enterToSend  bool
}

func newChatEntry(onSend func()) *chatEntry {
	e := &chatEntry{onSend: onSend, enterToSend: true}
	e.ExtendBaseWidget(e)
	e.MultiLine = true
	e.Wrapping = fyne.TextWrapWord
	e.SetPlaceHolder("输入消息...")
	return e
}

func (e *chatEntry) TypedKey(event *fyne.KeyEvent) {
	if e.enterToSend && (event.Name == fyne.KeyReturn || event.Name == fyne.KeyEnter) {
		if e.onSend != nil {
			e.onSend()
		}
		return
	}
	e.Entry.TypedKey(event)
}

func (e *chatEntry) TypedShortcut(shortcut fyne.Shortcut) {
	if _, ok := shortcut.(*fyne.ShortcutPaste); ok {
		if e.onPasteImage != nil && e.onPasteImage() {
			return
		}
	}
	e.Entry.TypedShortcut(shortcut)
}

// readOnlyEntry 是气泡内展示文本的只读 Entry：
// 保留 widget.Entry 的鼠标选择 + 系统级 Ctrl/Cmd+C 复制能力，
// 但屏蔽打字、粘贴、剪切等写入操作。
type readOnlyEntry struct {
	widget.Entry
}

func newReadOnlyEntry() *readOnlyEntry {
	e := &readOnlyEntry{}
	e.ExtendBaseWidget(e)
	e.MultiLine = true
	// TextWrapBreak：任何超过宽度的一行都在字符边界硬换行，
	// 避免长串没空格/单词时溢出气泡。
	e.Wrapping = fyne.TextWrapBreak
	// 默认多行 Entry 最少 3 行，单行消息会撑出很大高度，降到 1 行。
	e.SetMinRowsVisible(1)
	return e
}

// TypedRune 丢弃所有可见字符输入。
func (e *readOnlyEntry) TypedRune(rune) {}

// TypedKey 只放行光标移动 / 选择相关的键，其它（退格、回车、删除等）忽略。
func (e *readOnlyEntry) TypedKey(ev *fyne.KeyEvent) {
	switch ev.Name {
	case fyne.KeyLeft, fyne.KeyRight, fyne.KeyUp, fyne.KeyDown,
		fyne.KeyHome, fyne.KeyEnd, fyne.KeyPageUp, fyne.KeyPageDown:
		e.Entry.TypedKey(ev)
	}
}

// TypedShortcut 只放行 Copy / SelectAll，屏蔽 Paste / Cut / Undo / Redo。
func (e *readOnlyEntry) TypedShortcut(s fyne.Shortcut) {
	switch s.(type) {
	case *fyne.ShortcutCopy, *fyne.ShortcutSelectAll:
		e.Entry.TypedShortcut(s)
	}
}

// imageTapper 是盖在气泡图片之上的透明点击层。
// 用 widget.Button 做点击层会在鼠标悬停时画一层灰色 overlay，
// 这里自己实现一个只响应 Tapped、鼠标移入只改光标的最小组件。
type imageTapper struct {
	widget.BaseWidget
	onTapped func()
}

func newImageTapper() *imageTapper {
	t := &imageTapper{}
	t.ExtendBaseWidget(t)
	return t
}

func (t *imageTapper) CreateRenderer() fyne.WidgetRenderer {
	r := canvas.NewRectangle(color.Transparent)
	return widget.NewSimpleRenderer(r)
}

func (t *imageTapper) Tapped(*fyne.PointEvent) {
	if t.onTapped != nil {
		t.onTapped()
	}
}

// 实现 desktop.Cursorable，鼠标移入变指针光标，但不画背景高亮。
func (t *imageTapper) Cursor() desktop.Cursor {
	return desktop.PointerCursor
}

// 空实现，仅为避免 Fyne 的 tap-hover 回退链对 MouseIn/Out 的推测导致上色。
func (t *imageTapper) MouseIn(*desktop.MouseEvent)    {}
func (t *imageTapper) MouseMoved(*desktop.MouseEvent) {}
func (t *imageTapper) MouseOut()                      {}
