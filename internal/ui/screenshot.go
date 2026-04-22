package ui

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"
)

// 标注工具类型
const (
	toolRect  = 0
	toolLine  = 1
	toolArrow = 2
)

type annotation struct {
	tool           int
	x1, y1, x2, y2 int
}

// ========== 截图标注画布 ==========

type drawCanvas struct {
	widget.BaseWidget
	baseImg     image.Image
	annotations []annotation
	tool        int
	drawing     bool
	startX      float32
	startY      float32
	endX        float32
	endY        float32
	drawColor   color.RGBA
	lineWidth   int
}

func newDrawCanvas(img image.Image) *drawCanvas {
	d := &drawCanvas{
		baseImg:   img,
		drawColor: color.RGBA{R: 255, G: 0, B: 0, A: 255},
		lineWidth: 2,
	}
	d.ExtendBaseWidget(d)
	return d
}

// desktop.Mouseable 接口
func (d *drawCanvas) MouseDown(event *desktop.MouseEvent) {
	d.drawing = true
	d.startX = event.Position.X
	d.startY = event.Position.Y
	d.endX = event.Position.X
	d.endY = event.Position.Y
}

func (d *drawCanvas) MouseUp(event *desktop.MouseEvent) {
	if !d.drawing {
		return
	}
	d.endX = event.Position.X
	d.endY = event.Position.Y
	d.drawing = false

	size := d.Size()
	bounds := d.baseImg.Bounds()
	scaleX := float64(bounds.Dx()) / float64(size.Width)
	scaleY := float64(bounds.Dy()) / float64(size.Height)

	a := annotation{
		tool: d.tool,
		x1:   int(float64(d.startX) * scaleX),
		y1:   int(float64(d.startY) * scaleY),
		x2:   int(float64(d.endX) * scaleX),
		y2:   int(float64(d.endY) * scaleY),
	}
	d.annotations = append(d.annotations, a)
	d.Refresh()
}

// desktop.Hoverable 接口
func (d *drawCanvas) MouseIn(*desktop.MouseEvent)  {}
func (d *drawCanvas) MouseOut()                     {}
func (d *drawCanvas) MouseMoved(event *desktop.MouseEvent) {
	if d.drawing {
		d.endX = event.Position.X
		d.endY = event.Position.Y
		d.Refresh()
	}
}

func (d *drawCanvas) undo() {
	if len(d.annotations) > 0 {
		d.annotations = d.annotations[:len(d.annotations)-1]
		d.Refresh()
	}
}

func (d *drawCanvas) renderImage() *image.RGBA {
	bounds := d.baseImg.Bounds()
	result := image.NewRGBA(bounds)
	draw.Draw(result, bounds, d.baseImg, bounds.Min, draw.Src)
	for _, a := range d.annotations {
		d.drawAnnotation(result, a)
	}
	return result
}

func (d *drawCanvas) renderWithPreview() *image.RGBA {
	result := d.renderImage()
	if d.drawing {
		size := d.Size()
		bounds := d.baseImg.Bounds()
		scaleX := float64(bounds.Dx()) / float64(size.Width)
		scaleY := float64(bounds.Dy()) / float64(size.Height)
		preview := annotation{
			tool: d.tool,
			x1:   int(float64(d.startX) * scaleX),
			y1:   int(float64(d.startY) * scaleY),
			x2:   int(float64(d.endX) * scaleX),
			y2:   int(float64(d.endY) * scaleY),
		}
		d.drawAnnotation(result, preview)
	}
	return result
}

func (d *drawCanvas) drawAnnotation(img *image.RGBA, a annotation) {
	col := d.drawColor
	w := d.lineWidth
	switch a.tool {
	case toolRect:
		drawThickLine(img, a.x1, a.y1, a.x2, a.y1, col, w)
		drawThickLine(img, a.x2, a.y1, a.x2, a.y2, col, w)
		drawThickLine(img, a.x2, a.y2, a.x1, a.y2, col, w)
		drawThickLine(img, a.x1, a.y2, a.x1, a.y1, col, w)
	case toolLine:
		drawThickLine(img, a.x1, a.y1, a.x2, a.y2, col, w)
	case toolArrow:
		drawThickLine(img, a.x1, a.y1, a.x2, a.y2, col, w)
		drawArrowHead(img, a.x1, a.y1, a.x2, a.y2, col, w)
	}
}

func (d *drawCanvas) CreateRenderer() fyne.WidgetRenderer {
	raster := canvas.NewRaster(func(w, h int) image.Image {
		return d.renderWithPreview()
	})
	raster.ScaleMode = canvas.ImageScaleSmooth
	return &drawCanvasRenderer{dc: d, raster: raster}
}

type drawCanvasRenderer struct {
	dc     *drawCanvas
	raster *canvas.Raster
}

func (r *drawCanvasRenderer) Layout(size fyne.Size) {
	r.raster.Resize(size)
	r.raster.Move(fyne.NewPos(0, 0))
}

func (r *drawCanvasRenderer) MinSize() fyne.Size {
	bounds := r.dc.baseImg.Bounds()
	w := float32(bounds.Dx())
	h := float32(bounds.Dy())
	// 限制最小尺寸
	if w > 800 {
		scale := 800 / w
		w = 800
		h *= scale
	}
	return fyne.NewSize(w, h)
}

func (r *drawCanvasRenderer) Refresh() {
	r.raster.Refresh()
}

func (r *drawCanvasRenderer) Objects() []fyne.CanvasObject {
	return []fyne.CanvasObject{r.raster}
}

func (r *drawCanvasRenderer) Destroy() {}

// ========== 绘图基础 ==========

func drawThickLine(img *image.RGBA, x1, y1, x2, y2 int, col color.RGBA, radius int) {
	dx := absInt(x2 - x1)
	dy := absInt(y2 - y1)
	sx, sy := 1, 1
	if x1 > x2 {
		sx = -1
	}
	if y1 > y2 {
		sy = -1
	}
	err := dx - dy
	x, y := x1, y1

	for {
		fillDot(img, x, y, radius, col)
		if x == x2 && y == y2 {
			break
		}
		e2 := 2 * err
		if e2 > -dy {
			err -= dy
			x += sx
		}
		if e2 < dx {
			err += dx
			y += sy
		}
	}
}

func fillDot(img *image.RGBA, cx, cy, radius int, col color.RGBA) {
	bounds := img.Bounds()
	for dy := -radius; dy <= radius; dy++ {
		for dx := -radius; dx <= radius; dx++ {
			px, py := cx+dx, cy+dy
			if px >= bounds.Min.X && px < bounds.Max.X && py >= bounds.Min.Y && py < bounds.Max.Y {
				img.SetRGBA(px, py, col)
			}
		}
	}
}

func drawArrowHead(img *image.RGBA, x1, y1, x2, y2 int, col color.RGBA, w int) {
	angle := math.Atan2(float64(y2-y1), float64(x2-x1))
	headLen := 20.0
	headAngle := math.Pi / 6.0

	ax := x2 - int(headLen*math.Cos(angle-headAngle))
	ay := y2 - int(headLen*math.Sin(angle-headAngle))
	bx := x2 - int(headLen*math.Cos(angle+headAngle))
	by := y2 - int(headLen*math.Sin(angle+headAngle))

	drawThickLine(img, x2, y2, ax, ay, col, w)
	drawThickLine(img, x2, y2, bx, by, col, w)
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// ========== 截图捕获 ==========

func (a *App) startScreenshot() {
	// macOS 首次使用提示屏幕录制权限
	if runtime.GOOS == "darwin" {
		shown, _ := a.store.GetSetting("screenshot_perm_hint")
		if shown != "1" {
			_ = a.store.SetSetting("screenshot_perm_hint", "1")
			dialog.ShowConfirm("截图权限提示",
				"截图功能需要「屏幕录制」权限。\n\n"+
					"如果截图只显示桌面壁纸，请前往：\n"+
					"系统设置 → 隐私与安全性 → 屏幕录制\n"+
					"为终端或本应用开启权限后重试。\n\n"+
					"是否继续截图？",
				func(ok bool) {
					if ok {
						a.doScreenshot()
					}
				}, a.window)
			return
		}
	}
	a.doScreenshot()
}

func (a *App) doScreenshot() {
	tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("xtx_screenshot_%d.png", time.Now().UnixNano()))

	// 先隐藏窗口，避免截到自己的窗口
	a.window.Hide()
	time.Sleep(500 * time.Millisecond)

	err := captureScreenRegion(tmpFile)

	// 截图完成后恢复窗口
	a.window.Show()

	if err != nil {
		if _, statErr := os.Stat(tmpFile); statErr != nil {
			return // 用户取消
		}
		dialog.ShowError(fmt.Errorf("截图失败: %v", err), a.window)
		return
	}

	if _, err := os.Stat(tmpFile); err != nil {
		return // 用户取消
	}

	a.showAnnotationEditor(tmpFile)
}

func captureScreenRegion(destPath string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("screencapture", "-i", destPath).Run()
	case "linux":
		tools := []struct {
			cmd  string
			args []string
		}{
			{"maim", []string{"-s", destPath}},
			{"gnome-screenshot", []string{"-a", "-f", destPath}},
			{"scrot", []string{"-s", destPath}},
		}
		for _, t := range tools {
			if _, err := exec.LookPath(t.cmd); err == nil {
				return exec.Command(t.cmd, t.args...).Run()
			}
		}
		return fmt.Errorf("请安装 maim, gnome-screenshot 或 scrot")
	case "windows":
		return exec.Command("snippingtool", "/clip").Run()
	default:
		return fmt.Errorf("不支持的操作系统: %s", runtime.GOOS)
	}
}

func (a *App) showAnnotationEditor(imgPath string) {
	f, err := os.Open(imgPath)
	if err != nil {
		dialog.ShowError(fmt.Errorf("打开截图失败: %v", err), a.window)
		return
	}
	defer f.Close()

	img, _, err := image.Decode(f)
	if err != nil {
		dialog.ShowError(fmt.Errorf("解码截图失败: %v", err), a.window)
		return
	}

	w := a.fyneApp.NewWindow("截图标注")
	dc := newDrawCanvas(img)

	// 工具按钮
	rectBtn := widget.NewButton("▭ 框选", nil)
	lineBtn := widget.NewButton("╱ 画线", nil)
	arrowBtn := widget.NewButton("→ 箭头", nil)

	// 高亮当前选中工具
	updateToolButtons := func() {
		rectBtn.Importance = widget.MediumImportance
		lineBtn.Importance = widget.MediumImportance
		arrowBtn.Importance = widget.MediumImportance
		switch dc.tool {
		case toolRect:
			rectBtn.Importance = widget.HighImportance
		case toolLine:
			lineBtn.Importance = widget.HighImportance
		case toolArrow:
			arrowBtn.Importance = widget.HighImportance
		}
		rectBtn.Refresh()
		lineBtn.Refresh()
		arrowBtn.Refresh()
	}

	rectBtn.OnTapped = func() { dc.tool = toolRect; updateToolButtons() }
	lineBtn.OnTapped = func() { dc.tool = toolLine; updateToolButtons() }
	arrowBtn.OnTapped = func() { dc.tool = toolArrow; updateToolButtons() }
	updateToolButtons()

	// 颜色选择
	type colorOption struct {
		name string
		col  color.RGBA
	}
	colors := []colorOption{
		{"红", color.RGBA{255, 0, 0, 255}},
		{"蓝", color.RGBA{0, 100, 255, 255}},
		{"绿", color.RGBA{0, 180, 0, 255}},
		{"黄", color.RGBA{255, 200, 0, 255}},
		{"黑", color.RGBA{0, 0, 0, 255}},
		{"白", color.RGBA{255, 255, 255, 255}},
	}

	var colorBtns []*widget.Button
	updateColorButtons := func() {
		for i, btn := range colorBtns {
			if colors[i].col == dc.drawColor {
				btn.Importance = widget.HighImportance
			} else {
				btn.Importance = widget.MediumImportance
			}
			btn.Refresh()
		}
	}

	colorBox := container.NewHBox()
	for _, c := range colors {
		c := c
		// 用色块 + 文字做按钮
		btn := widget.NewButton(c.name, nil)
		btn.OnTapped = func() {
			dc.drawColor = c.col
			updateColorButtons()
		}
		colorBtns = append(colorBtns, btn)
		colorBox.Add(btn)
	}
	updateColorButtons()

	undoBtn := widget.NewButton("↩ 撤销", func() { dc.undo() })
	cancelBtn := widget.NewButton("取消", func() {
		w.Close()
		os.Remove(imgPath)
	})

	confirmBtn := widget.NewButton("确定", func() {
		result := dc.renderImage()
		outPath := filepath.Join(os.TempDir(), fmt.Sprintf("xtx_annotated_%d.png", time.Now().UnixNano()))
		outF, err := os.Create(outPath)
		if err != nil {
			return
		}
		_ = png.Encode(outF, result)
		outF.Close()

		// 复制到剪贴板
		_ = clipboardWriteImage(outPath)

		// 发送到当前聊天
		a.sendImageFromPath(outPath)

		w.Close()
		os.Remove(imgPath)
	})
	confirmBtn.Importance = widget.HighImportance

	toolbar := container.NewHBox(
		rectBtn, lineBtn, arrowBtn,
		widget.NewSeparator(),
		colorBox,
		widget.NewSeparator(),
		undoBtn,
		layout.NewSpacer(),
		cancelBtn, confirmBtn,
	)

	// 窗口尺寸
	bounds := img.Bounds()
	winW := float32(bounds.Dx())
	winH := float32(bounds.Dy()) + 50
	if winW > 1200 {
		winW = 1200
	}
	if winH > 800 {
		winH = 800
	}
	if winW < 400 {
		winW = 400
	}
	if winH < 300 {
		winH = 300
	}

	content := container.NewBorder(toolbar, nil, nil, nil, dc)
	w.SetContent(content)
	w.Resize(fyne.NewSize(winW, winH))
	w.Show()
}

// ========== 剪贴板操作 ==========

func clipboardWriteImage(imgPath string) error {
	switch runtime.GOOS {
	case "darwin":
		script := fmt.Sprintf(`set the clipboard to (read (POSIX file "%s") as «class PNGf»)`, imgPath)
		return exec.Command("osascript", "-e", script).Run()
	case "linux":
		if _, err := exec.LookPath("xclip"); err == nil {
			return exec.Command("xclip", "-selection", "clipboard", "-t", "image/png", "-i", imgPath).Run()
		}
		if _, err := exec.LookPath("wl-copy"); err == nil {
			data, err := os.ReadFile(imgPath)
			if err != nil {
				return err
			}
			cmd := exec.Command("wl-copy", "--type", "image/png")
			cmd.Stdin = strings.NewReader(string(data))
			return cmd.Run()
		}
		return fmt.Errorf("请安装 xclip 或 wl-copy")
	case "windows":
		return exec.Command("powershell", "-Command",
			fmt.Sprintf(`Set-Clipboard -Path "%s"`, imgPath)).Run()
	default:
		return fmt.Errorf("不支持的操作系统")
	}
}

func clipboardReadImage(destPath string) error {
	switch runtime.GOOS {
	case "darwin":
		script := fmt.Sprintf(`try
	set pngData to the clipboard as «class PNGf»
	set filePath to "%s"
	set fileRef to open for access filePath with write permission
	write pngData to fileRef
	close access fileRef
	return "ok"
on error
	return "no"
end try`, destPath)
		out, err := exec.Command("osascript", "-e", script).Output()
		if err != nil || strings.TrimSpace(string(out)) != "ok" {
			return fmt.Errorf("剪贴板中没有图片")
		}
		return nil
	case "linux":
		if _, err := exec.LookPath("xclip"); err == nil {
			out, err := exec.Command("xclip", "-selection", "clipboard", "-t", "image/png", "-o").Output()
			if err != nil || len(out) == 0 {
				return fmt.Errorf("剪贴板中没有图片")
			}
			return os.WriteFile(destPath, out, 0644)
		}
		return fmt.Errorf("请安装 xclip")
	case "windows":
		ps := fmt.Sprintf(`$img = Get-Clipboard -Format Image
if ($img -ne $null) {
	$img.Save("%s")
	Write-Output "ok"
} else {
	Write-Output "no"
}`, destPath)
		out, err := exec.Command("powershell", "-Command", ps).Output()
		if err != nil || strings.TrimSpace(string(out)) != "ok" {
			return fmt.Errorf("剪贴板中没有图片")
		}
		return nil
	default:
		return fmt.Errorf("不支持的操作系统")
	}
}

// ========== 快捷键解析 ==========

func parseShortcut(s string) *desktop.CustomShortcut {
	parts := strings.Split(strings.ToLower(s), "+")
	if len(parts) < 2 {
		return nil
	}

	var mod fyne.KeyModifier
	var keyName fyne.KeyName

	for _, part := range parts {
		part = strings.TrimSpace(part)
		switch part {
		case "ctrl":
			mod |= fyne.KeyModifierControl
		case "shift":
			mod |= fyne.KeyModifierShift
		case "alt":
			mod |= fyne.KeyModifierAlt
		case "super", "cmd":
			mod |= fyne.KeyModifierSuper
		default:
			if len(part) == 1 {
				keyName = fyne.KeyName(strings.ToUpper(part))
			}
		}
	}

	if keyName == "" || mod == 0 {
		return nil
	}

	return &desktop.CustomShortcut{KeyName: keyName, Modifier: mod}
}
