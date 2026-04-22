package ui

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
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

// borderlessEntryTheme 无边框输入框主题
type borderlessEntryTheme struct {
	parent fyne.Theme
}

func (t *borderlessEntryTheme) Color(name fyne.ThemeColorName, variant fyne.ThemeVariant) color.Color {
	if name == theme.ColorNameInputBorder {
		return color.Transparent
	}
	// Fyne 的光标使用 ColorNamePrimary 绘制，某些系统主题下这个色值
	// 与输入背景过近导致光标不可见。这里强制给出高对比度的蓝色。
	if name == theme.ColorNamePrimary {
		if variant == theme.VariantDark {
			return color.NRGBA{R: 120, G: 190, B: 255, A: 255}
		}
		return color.NRGBA{R: 25, G: 118, B: 210, A: 255}
	}
	return t.parent.Color(name, variant)
}

func (t *borderlessEntryTheme) Font(style fyne.TextStyle) fyne.Resource {
	return t.parent.Font(style)
}

func (t *borderlessEntryTheme) Icon(name fyne.ThemeIconName) fyne.Resource {
	return t.parent.Icon(name)
}

func (t *borderlessEntryTheme) Size(name fyne.ThemeSizeName) float32 {
	if name == theme.SizeNameInputBorder {
		return 0
	}
	return t.parent.Size(name)
}
