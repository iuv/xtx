package ui

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
)

var commonEmojis = []string{
	// 表情
	"😀", "😁", "😂", "🤣", "😄", "😅", "😆", "😉", "😊", "😋",
	"😎", "😍", "😘", "🥰", "😗", "😚", "😛", "😝", "😜", "🤪",
	"🤨", "🧐", "🤓", "😏", "🥳", "🤩", "😔", "😟", "😕", "🙁",
	"😣", "😖", "😫", "😩", "🥺", "😢", "😭", "😤", "😡", "🤬",
	"😈", "👿", "💀", "💩", "🤡", "👻", "🙈", "🙉", "🙊", "😺",
	// 手势
	"👍", "👎", "👊", "✊", "🤛", "🤜", "🤞", "✌️", "🤟", "🤘",
	"👌", "👈", "👉", "👆", "👇", "☝️", "✋", "🤚", "🖐️", "🖖",
	"👋", "🤙", "💪", "🙏", "👏", "🤝", "🖕", "✍️", "🫶", "🤲",
	// 符号
	"❤️", "🧡", "💛", "💚", "💙", "💜", "🖤", "🤍", "💔", "❣️",
	"💯", "✨", "🔥", "⭐", "🌟", "💫", "🎉", "🎊", "🎈", "🎁",
	"💬", "💭", "👀", "🏆", "🌹", "🌸", "🍀", "☕", "🍻", "🎵",
	"✅", "❌", "⚠️", "💡", "📌", "📎", "🔔", "🚀", "💻", "📱",
}

func (a *App) showEmojiPicker() {
	w := a.fyneApp.NewWindow("表情")
	w.Resize(fyne.NewSize(460, 420))

	cellSize := fyne.NewSize(52, 52)
	var items []fyne.CanvasObject
	for _, emoji := range commonEmojis {
		e := emoji
		label := canvas.NewText(e, color.Transparent)
		label.TextSize = 28
		label.Alignment = fyne.TextAlignCenter
		btn := widget.NewButton("", func() {
			a.chatInput.SetText(a.chatInput.Text + e)
			w.Close()
		})
		btn.Importance = widget.LowImportance
		cell := container.NewStack(btn, container.NewCenter(label))
		items = append(items, cell)
	}

	grid := container.NewGridWrap(cellSize, items...)
	scroll := container.NewVScroll(grid)
	w.SetContent(scroll)
	w.Show()
}
