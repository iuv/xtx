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

