package ui

import (
	"fyne.io/fyne/v2"
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
	e.Wrapping = fyne.TextWrapWord
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
