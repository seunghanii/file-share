//go:build windows

// PC용 프로그램 창과 작업표시줄 트레이 아이콘.
// 창의 X(닫기)를 누르면 트레이로 숨고, 트레이 메뉴의 "종료"로만 완전히 꺼집니다.
package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
	qrcode "github.com/skip2/go-qrcode"
)

var (
	mw       *walk.MainWindow
	ni       *walk.NotifyIcon
	quitting atomic.Bool
)

// ---------- 파일 목록 ----------

func signature(files []fileInfo) string {
	var b strings.Builder
	for _, f := range files {
		fmt.Fprintf(&b, "%s|%d|%f|%s\n", f.Name, f.Size, f.Mtime, f.ClipID)
	}
	return b.String()
}

func humanSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.0f KB", float64(n)/1024)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.1f MB", float64(n)/1024/1024)
	default:
		return fmt.Sprintf("%.2f GB", float64(n)/1024/1024/1024)
	}
}

// ---------- 윈도우 기능 ----------

func shellOpen(path string) {
	verb, _ := syscall.UTF16PtrFromString("open")
	file, _ := syscall.UTF16PtrFromString(path)
	win.ShellExecute(0, verb, file, nil, nil, win.SW_SHOWNORMAL)
}

func revealInFolder(path string) {
	exec.Command("explorer", "/select,", path).Start()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	out.Close()
	return os.Rename(tmp, dst)
}

func showWindow() {
	if mw == nil {
		return
	}
	mw.Synchronize(func() {
		mw.Show()
		win.ShowWindow(mw.Handle(), win.SW_RESTORE)
		win.SetForegroundWindow(mw.Handle())
	})
}

func quitApp() {
	quitting.Store(true)
	if mw == nil {
		os.Exit(0)
	}
	mw.Synchronize(func() {
		if ni != nil {
			ni.Dispose()
		}
		mw.Close()
		walk.App().Exit(0)
	})
	go func() { time.Sleep(2 * time.Second); os.Exit(0) }() // 안전장치
}

func showError(msg string) {
	walk.MsgBox(nil, "파일공유", msg, walk.MsgBoxIconError)
}

// ---------- 디자인 ----------

var (
	colBg      = walk.RGB(243, 244, 246)
	colCard    = walk.RGB(255, 255, 255)
	colText    = walk.RGB(17, 24, 39)
	colMuted   = walk.RGB(107, 114, 128)
	colBorder  = walk.RGB(229, 231, 235)
	colAccent  = walk.RGB(37, 99, 235)
	colAccentD = walk.RGB(29, 78, 216)
	colDanger  = walk.RGB(220, 38, 38)
	colGreen   = walk.RGB(22, 163, 74)
)

const uiFont = "Malgun Gothic"

// 브러시/펜/글꼴은 한 번 만들고 재사용 (매번 만들면 GDI 핸들이 계속 늘어남)
var (
	brushCache = map[walk.Color]*walk.SolidColorBrush{}
	penCache   = map[walk.Color]*walk.CosmeticPen{}
	fontCache  = map[string]*walk.Font{}
)

func mustBrush(c walk.Color) *walk.SolidColorBrush {
	if b, ok := brushCache[c]; ok {
		return b
	}
	b, _ := walk.NewSolidColorBrush(c)
	brushCache[c] = b
	return b
}

func mustPen(c walk.Color) *walk.CosmeticPen {
	if p, ok := penCache[c]; ok {
		return p
	}
	p, _ := walk.NewCosmeticPen(walk.PenSolid, c)
	penCache[c] = p
	return p
}

func mustFont(size int, bold bool) *walk.Font {
	key := fmt.Sprint(size, bold)
	if f, ok := fontCache[key]; ok {
		return f
	}
	var st walk.FontStyle
	if bold {
		st = walk.FontBold
	}
	f, _ := walk.NewFont(uiFont, size, st)
	fontCache[key] = f
	return f
}

// 창의 실제 DPI로 메모리에 그린 뒤 한 번에 화면에 복사.
// (walk 기본 버퍼링은 시스템 DPI로 글자를 그려서, 화면 배율이 다른 모니터에서 글자가 커짐)
func paintAtWindowDPI(cw *walk.CustomWidget, target *walk.Canvas, draw func(c *walk.Canvas, b walk.Rectangle) error) error {
	b := cw.ClientBoundsPixels()
	if b.Width < 1 || b.Height < 1 {
		return nil
	}
	bmp, err := walk.NewBitmapForDPI(walk.Size{Width: b.Width, Height: b.Height}, cw.DPI())
	if err != nil {
		return draw(target, b)
	}
	defer bmp.Dispose()
	buf, err := walk.NewCanvasFromImage(bmp)
	if err != nil {
		return draw(target, b)
	}
	err = draw(buf, b)
	win.BitBlt(target.HDC(), 0, 0, int32(b.Width), int32(b.Height), buf.HDC(), 0, 0, win.SRCCOPY)
	buf.Dispose()
	return err
}

// 글자 수로 버튼 폭 대략 계산 (한글은 넓게)
func textWidth(s string, px int) int {
	w := 0
	for _, r := range s {
		if r > 0x2000 {
			w += px
		} else {
			w += px * 6 / 10
		}
	}
	return w
}

type btnKind int

const (
	btnPrimary btnKind = iota
	btnSecondary
	btnDanger
)

// 둥근 모서리의 평평한 버튼 (직접 그림)
func flatButton(text string, kind btnKind, parentBg walk.Color, onClick func()) Widget {
	var cw *walk.CustomWidget
	pressed := false
	w := textWidth(text, 11) + 22
	return CustomWidget{
		AssignTo:            &cw,
		MinSize:             Size{Width: w, Height: 28},
		MaxSize:             Size{Width: w, Height: 28},
		InvalidatesOnResize: true,
		PaintMode:           PaintNoErase,
		PaintPixels: func(target *walk.Canvas, _ walk.Rectangle) error {
			return paintAtWindowDPI(cw, target, func(c *walk.Canvas, b walk.Rectangle) error {
				c.FillRectanglePixels(mustBrush(parentBg), b)
				r := walk.Rectangle{X: 0, Y: 0, Width: b.Width - 1, Height: b.Height - 1}
				rad := walk.Size{Width: b.Height / 3, Height: b.Height / 3}
				fg := colText
				switch kind {
				case btnPrimary:
					bg := colAccent
					if pressed {
						bg = colAccentD
					}
					c.FillRoundedRectanglePixels(mustBrush(bg), r, rad)
					fg = colCard
				default:
					bg := colCard
					if pressed {
						bg = colBg
					}
					c.FillRoundedRectanglePixels(mustBrush(bg), r, rad)
					c.DrawRoundedRectanglePixels(mustPen(colBorder), r, rad)
					if kind == btnDanger {
						fg = colDanger
					}
				}
				return c.DrawTextPixels(text, mustFont(9, false), fg, b, walk.TextCenter|walk.TextVCenter|walk.TextSingleLine)
			})
		},
		OnMouseDown: func(x, y int, button walk.MouseButton) {
			if button == walk.LeftButton {
				pressed = true
				cw.Invalidate()
			}
		},
		OnMouseUp: func(x, y int, button walk.MouseButton) {
			if !pressed {
				return
			}
			pressed = false
			cw.Invalidate()
			b := cw.ClientBoundsPixels()
			if x >= 0 && y >= 0 && x < b.Width && y < b.Height && onClick != nil {
				onClick()
			}
		},
	}
}

// 확장자별 색상 배지 (목록 왼쪽 아이콘)
var badgeColors = map[string]walk.Color{
	"pdf": walk.RGB(229, 37, 42), "xls": walk.RGB(29, 112, 68), "xlsx": walk.RGB(29, 112, 68), "csv": walk.RGB(29, 112, 68),
	"ppt": walk.RGB(210, 71, 38), "pptx": walk.RGB(210, 71, 38), "doc": walk.RGB(43, 87, 154), "docx": walk.RGB(43, 87, 154),
	"hwp": walk.RGB(31, 138, 224), "zip": walk.RGB(122, 90, 248), "txt": walk.RGB(107, 114, 128),
	"jpg": walk.RGB(13, 148, 136), "jpeg": walk.RGB(13, 148, 136), "png": walk.RGB(13, 148, 136), "heic": walk.RGB(13, 148, 136),
	"text": walk.RGB(124, 58, 237), "gif": walk.RGB(13, 148, 136), "webp": walk.RGB(13, 148, 136), "mp4": walk.RGB(219, 39, 119), "mov": walk.RGB(219, 39, 119),
}

// 둥근 사각형 이미지 (부드러운 모서리, bg 위에 합성)
func roundedSquare(size int, c, bg walk.Color) *image.RGBA {
	return roundedImage(size, bg, func(x, y int) (r, g, b uint8) { return c.R(), c.G(), c.B() })
}

// 썸네일을 둥근 모서리로 (bg 위에 합성)
func roundedThumb(t *image.RGBA, bg walk.Color) *image.RGBA {
	return roundedImage(t.Bounds().Dx(), bg, func(x, y int) (r, g, b uint8) {
		i := t.PixOffset(x, y)
		return t.Pix[i], t.Pix[i+1], t.Pix[i+2]
	})
}

func roundedImage(size int, bg walk.Color, fill func(x, y int) (r, g, b uint8)) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	s := float64(size)
	r := s * 0.24
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			hit := 0
			for sy := 0; sy < 4; sy++ {
				for sx := 0; sx < 4; sx++ {
					fx, fy := float64(x)+(float64(sx)+.5)/4, float64(y)+(float64(sy)+.5)/4
					cx := math.Max(r, math.Min(fx, s-r))
					cy := math.Max(r, math.Min(fy, s-r))
					if (fx-cx)*(fx-cx)+(fy-cy)*(fy-cy) <= r*r {
						hit++
					}
				}
			}
			a := float64(hit) / 16
			mix := func(f, b byte) uint8 { return uint8(float64(f)*a + float64(b)*(1-a)) }
			cr, cg, cb := fill(x, y)
			img.Set(x, y, color.RGBA{mix(cr, bg.R()), mix(cg, bg.G()), mix(cb, bg.B()), 255})
		}
	}
	return img
}

var badgeCache = map[string]*walk.Bitmap{}

func badgeBitmap(ext string, size int, bg walk.Color) *walk.Bitmap {
	key := fmt.Sprintf("%s|%d|%d", ext, size, bg)
	if b, ok := badgeCache[key]; ok {
		return b
	}
	col, ok := badgeColors[ext]
	if !ok {
		col = walk.RGB(142, 142, 147)
	}
	bmp, _ := walk.NewBitmapFromImageForDPI(roundedSquare(size, col, bg), 96)
	badgeCache[key] = bmp
	return bmp
}

func extOf(name string) string {
	return strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
}

// ---------- 파일 목록 (직접 그리는 카드형 목록) ----------

type fileList struct {
	cw       *walk.CustomWidget
	items    []fileInfo
	offset   int // 세로 스크롤 위치 (px)
	dragY    int // 스크롤바 드래그 시작점 (-1 = 드래그 아님)
	dragOff  int
	sel      map[string]bool
	anchor   int
	lastRow  int
	lastTime time.Time
	onOpen   func()
	onChange func()
	onCopy   func(f fileInfo) // 행의 [복사] 버튼
	copied   string           // 방금 복사한 행 (잠깐 "복사됨" 표시)
	thumbs   map[string]*walk.Bitmap // 이미지 썸네일 (nil = 만들 수 없음)
	loading  map[string]bool
}

var thumbSem = make(chan struct{}, 2) // 썸네일은 동시에 2개까지만 만듦

// 이미지 행의 썸네일. 아직 없으면 백그라운드에서 만들고 nil 반환 (그동안 배지 표시)
func (l *fileList) thumbFor(f fileInfo, size int, bg walk.Color) *walk.Bitmap {
	key := fmt.Sprintf("%s|%f|%d|%d", f.Name, f.Mtime, size, bg)
	if b, ok := l.thumbs[key]; ok {
		return b
	}
	if l.loading[key] {
		return nil
	}
	l.loading[key] = true
	path := filepath.Join(shareDir, f.Name)
	go func() {
		thumbSem <- struct{}{}
		var img *image.RGBA
		if t, err := thumbnail(path, size); err == nil {
			img = roundedThumb(t.(*image.RGBA), bg)
		}
		<-thumbSem
		l.cw.Synchronize(func() {
			delete(l.loading, key)
			var bmp *walk.Bitmap
			if img != nil {
				bmp, _ = walk.NewBitmapFromImageForDPI(img, 96)
			}
			if len(l.thumbs) > 300 {
				for k, b := range l.thumbs {
					if b != nil {
						b.Dispose()
					}
					delete(l.thumbs, k)
				}
			}
			l.thumbs[key] = bmp
			l.cw.Invalidate()
		})
	}()
	return nil
}

// 이 행에 [복사] 버튼이 있는지 (텍스트 클립, 이미지)
func canCopy(f fileInfo) bool { return f.ClipID != "" || isCopyableImage(f.Name) }

// 행의 [복사] 버튼 위치
func (l *fileList) copyBtn(row int) walk.Rectangle {
	b := l.cw.ClientBoundsPixels()
	w, h := l.px(60), l.px(26)
	return walk.Rectangle{X: b.Width - l.px(24) - w, Y: row*l.rowH() - l.offset + (l.rowH()-h)/2, Width: w, Height: h}
}

func (l *fileList) rowH() int    { return 56 * l.cw.DPI() / 96 }
func (l *fileList) px(v int) int { return v * l.cw.DPI() / 96 }

func (l *fileList) setItems(items []fileInfo) {
	l.items = items
	keep := map[string]bool{}
	for _, f := range items {
		if l.sel[f.Name] {
			keep[f.Name] = true
		}
	}
	l.sel = keep
	l.clamp()
	l.cw.Invalidate()
}

func (l *fileList) contentH() int { return len(l.items) * l.rowH() }

func (l *fileList) clamp() {
	max := l.contentH() - l.cw.ClientBoundsPixels().Height
	if l.offset > max {
		l.offset = max
	}
	if l.offset < 0 {
		l.offset = 0
	}
}

// 스크롤바 위치 (내용이 넘칠 때만)
func (l *fileList) thumb() (walk.Rectangle, bool) {
	b := l.cw.ClientBoundsPixels()
	ch := l.contentH()
	if ch <= b.Height+l.px(8) {
		return walk.Rectangle{}, false
	}
	th := b.Height * b.Height / ch
	if th < l.px(30) {
		th = l.px(30)
	}
	ty := (b.Height - th) * l.offset / (ch - b.Height)
	return walk.Rectangle{X: b.Width - l.px(10), Y: ty, Width: l.px(6), Height: th}, true
}

func (l *fileList) wheel(delta int) {
	l.offset -= delta * l.rowH() / 120 * 2
	l.clamp()
	l.cw.Invalidate()
}

func (l *fileList) mouseMove(x, y int, button walk.MouseButton) {
	if l.dragY < 0 || button&walk.LeftButton == 0 {
		return
	}
	b := l.cw.ClientBoundsPixels()
	t, ok := l.thumb()
	if !ok {
		return
	}
	l.offset = l.dragOff + (y-l.dragY)*(l.contentH()-b.Height)/maxInt(1, b.Height-t.Height)
	l.clamp()
	l.cw.Invalidate()
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (l *fileList) selected() []fileInfo {
	var out []fileInfo
	for _, f := range l.items {
		if l.sel[f.Name] {
			out = append(out, f)
		}
	}
	return out
}

func (l *fileList) rowAt(y int) int {
	r := (y + l.offset) / l.rowH()
	if r < 0 || r >= len(l.items) {
		return -1
	}
	return r
}

func (l *fileList) click(x, y int, button walk.MouseButton) {
	l.cw.SetFocus()
	if t, ok := l.thumb(); ok && button == walk.LeftButton && x >= t.X-l.px(6) {
		l.dragY, l.dragOff = y, l.offset
		return
	}
	row := l.rowAt(y)
	if row >= 0 && button == walk.LeftButton && canCopy(l.items[row]) {
		if r := l.copyBtn(row); x >= r.X && x < r.X+r.Width && y >= r.Y && y < r.Y+r.Height {
			f := l.items[row]
			l.onCopy(f)
			l.copied = f.Name
			l.cw.Invalidate()
			go func() {
				time.Sleep(1200 * time.Millisecond)
				l.cw.Synchronize(func() {
					if l.copied == f.Name {
						l.copied = ""
						l.cw.Invalidate()
					}
				})
			}()
			return
		}
	}
	if row < 0 {
		if button == walk.LeftButton {
			l.sel = map[string]bool{}
		}
		l.cw.Invalidate()
		l.onChange()
		return
	}
	name := l.items[row].Name
	switch {
	case button == walk.RightButton:
		if !l.sel[name] {
			l.sel = map[string]bool{name: true}
			l.anchor = row
		}
	case walk.ControlDown():
		if l.sel[name] {
			delete(l.sel, name)
		} else {
			l.sel[name] = true
		}
		l.anchor = row
	case walk.ShiftDown():
		l.sel = map[string]bool{}
		a, b := l.anchor, row
		if a > b {
			a, b = b, a
		}
		for i := a; i <= b && i < len(l.items); i++ {
			l.sel[l.items[i].Name] = true
		}
	default:
		l.sel = map[string]bool{name: true}
		l.anchor = row
		// 같은 줄을 빠르게 두 번 누르면 열기
		if row == l.lastRow && time.Since(l.lastTime) < 450*time.Millisecond {
			l.lastTime = time.Time{}
			l.cw.Invalidate()
			l.onOpen()
			return
		}
		l.lastRow, l.lastTime = row, time.Now()
	}
	l.cw.Invalidate()
	l.onChange()
}

func (l *fileList) paint(target *walk.Canvas, _ walk.Rectangle) error {
	return paintAtWindowDPI(l.cw, target, l.draw)
}

func (l *fileList) draw(c *walk.Canvas, b walk.Rectangle) error {
	c.FillRectanglePixels(mustBrush(colCard), b)
	nameFont := mustFont(10, false)
	metaFont := mustFont(8, false)
	extFont := mustFont(6, true)

	if len(l.items) == 0 {
		r := walk.Rectangle{Y: l.px(60), Width: b.Width, Height: l.px(30)}
		c.DrawTextPixels("아직 공유된 항목이 없어요", mustFont(10, true), colText, r, walk.TextCenter|walk.TextSingleLine)
		r.Y += l.px(30)
		c.DrawTextPixels("파일을 끌어다 놓거나, Ctrl+V로 캡처·텍스트를 붙여넣거나, [＋ 파일 추가]를 누르세요", metaFont, colMuted, r, walk.TextCenter|walk.TextSingleLine)
		return nil
	}

	rh := l.rowH()
	pad := l.px(14)
	bs := l.px(38)
	line := mustPen(colBg)
	l.clamp()
	for i, f := range l.items {
		y := i*rh - l.offset
		if y+rh < 0 || y > b.Height {
			continue
		}
		bg := colCard
		if l.sel[f.Name] {
			bg = walk.RGB(239, 246, 255)
			c.FillRectanglePixels(mustBrush(bg), walk.Rectangle{X: 0, Y: y, Width: b.Width, Height: rh})
			c.FillRectanglePixels(mustBrush(colAccent), walk.Rectangle{X: 0, Y: y, Width: l.px(3), Height: rh})
		}
		if i > 0 {
			c.DrawLinePixels(line, walk.Point{X: pad, Y: y}, walk.Point{X: b.Width - pad, Y: y})
		}
		ext := extOf(f.Name)
		if f.ClipID != "" {
			ext = "text"
		}
		br := walk.Rectangle{X: pad, Y: y + (rh-bs)/2, Width: bs, Height: bs}
		var tb *walk.Bitmap
		if f.ClipID == "" && hasThumb(f.Name) {
			tb = l.thumbFor(f, bs, bg)
		}
		if tb != nil {
			c.DrawImageStretchedPixels(tb, br)
		} else {
			c.DrawImageStretchedPixels(badgeBitmap(ext, bs, bg), br)
			label := strings.ToUpper(ext)
			if f.ClipID != "" {
				label = "Aa"
			}
			if label == "" {
				label = "FILE"
			}
			if len(label) > 4 {
				label = label[:4]
			}
			font := extFont
			if f.ClipID != "" {
				font = mustFont(11, true)
			}
			c.DrawTextPixels(label, font, colCard, br, walk.TextCenter|walk.TextVCenter|walk.TextSingleLine)
		}

		tx := pad + bs + l.px(12)
		tw := b.Width - tx - pad - l.px(10)
		if canCopy(f) {
			tw -= l.px(76)
		}
		title := f.Name
		if f.ClipID != "" {
			title = preview(f.Text, 120)
		}
		c.DrawTextPixels(title, nameFont, colText,
			walk.Rectangle{X: tx, Y: y + l.px(10), Width: tw, Height: l.px(19)}, walk.TextLeft|walk.TextSingleLine|walk.TextEndEllipsis)
		meta := humanSize(f.Size) + "  ·  " + time.Unix(int64(f.Mtime), 0).Format("2006-01-02 15:04")
		if f.ClipID != "" {
			meta = fmt.Sprintf("텍스트 %d자  ·  %s", len([]rune(f.Text)), time.Unix(int64(f.Mtime), 0).Format("2006-01-02 15:04"))
		}
		c.DrawTextPixels(meta, metaFont, colMuted,
			walk.Rectangle{X: tx, Y: y + l.px(30), Width: tw, Height: l.px(16)}, walk.TextLeft|walk.TextSingleLine|walk.TextEndEllipsis)

		if canCopy(f) {
			r := l.copyBtn(i)
			rr := walk.Rectangle{X: r.X, Y: r.Y, Width: r.Width - 1, Height: r.Height - 1}
			rad := walk.Size{Width: r.Height / 2, Height: r.Height / 2}
			text, fg := "복사", colAccent
			if l.copied == f.Name {
				text, fg = "복사됨", colCard
				c.FillRoundedRectanglePixels(mustBrush(colGreen), rr, rad)
			} else {
				c.FillRoundedRectanglePixels(mustBrush(bg), rr, rad)
				c.DrawRoundedRectanglePixels(mustPen(colAccent), rr, rad)
			}
			c.DrawTextPixels(text, mustFont(9, false), fg, r, walk.TextCenter|walk.TextVCenter|walk.TextSingleLine)
		}
	}
	if t, ok := l.thumb(); ok {
		c.FillRoundedRectanglePixels(mustBrush(walk.RGB(209, 213, 219)), t, walk.Size{Width: t.Width, Height: t.Width})
	}
	return nil
}

// ---------- 화면 ----------

func runGUI() {
	list := &fileList{sel: map[string]bool{}, lastRow: -1, dragY: -1,
		thumbs: map[string]*walk.Bitmap{}, loading: map[string]bool{}}
	var (
		status   *walk.Label
		countLbl *walk.Label
	)

	img, _ := png.Decode(bytes.NewReader(iconPNG))
	appIcon, _ := walk.NewIconFromImageForDPI(img, 96)
	curIP := localIP()
	qrBmp = makeQR(lanURL())
	var addrLbl *walk.Label

	setStatus := func(s string) { status.SetText(s) }

	refresh := func(force bool) {
		files := []fileInfo{}
		for _, c := range listClips() {
			files = append(files, fileInfo{Name: "clip:" + c.ID, ClipID: c.ID, Text: c.Text, Mtime: c.Time})
		}
		files = append(files, readFiles()...)
		sort.SliceStable(files, func(i, j int) bool { return files[i].Mtime > files[j].Mtime })
		if !force && signature(files) == signature(list.items) {
			return
		}
		list.setItems(files)
		if len(files) == 0 {
			countLbl.SetText("")
		} else {
			countLbl.SetText(fmt.Sprintf("%d개", len(files)))
		}
	}

	// [복사] 버튼 / Ctrl+C: 텍스트는 글자 그대로, 이미지는 그림으로 PC 클립보드에
	copyItem := func(f fileInfo) {
		if f.ClipID != "" {
			walk.Clipboard().SetText(strings.ReplaceAll(f.Text, "\n", "\r\n"))
			setStatus("텍스트를 복사했어요. 원하는 곳에 Ctrl+V 하세요.")
			return
		}
		if err := copyImageToClipboard(mw.Handle(), filepath.Join(shareDir, f.Name)); err != nil {
			setStatus(err.Error())
			return
		}
		setStatus("이미지를 복사했어요. 원하는 곳에 Ctrl+V 하세요.")
	}
	list.onCopy = copyItem

	// 클립은 파일 작업(열기/저장/폴더)에서 제외
	filesOnly := func(items []fileInfo) []fileInfo {
		var out []fileInfo
		for _, f := range items {
			if f.ClipID == "" {
				out = append(out, f)
			}
		}
		return out
	}

	addFiles := func(paths []string) {
		go func() {
			ok := 0
			for _, p := range paths {
				st, err := os.Stat(p)
				if err != nil || st.IsDir() {
					continue
				}
				mw.Synchronize(func() { setStatus("복사 중 · " + filepath.Base(p)) })
				if copyFile(p, uniquePath(safeName(filepath.Base(p)))) == nil {
					ok++
				}
			}
			mw.Synchronize(func() {
				setStatus(fmt.Sprintf("%d개 파일을 추가했어요. 다른 기기에서 바로 받을 수 있어요.", ok))
				refresh(true)
			})
		}()
	}

	openSelected := func() {
		sel := list.selected()
		if len(sel) == 0 {
			setStatus("열 파일을 목록에서 선택하세요.")
		}
		for _, f := range sel {
			if f.ClipID != "" {
				copyItem(f)
				continue
			}
			shellOpen(filepath.Join(shareDir, f.Name))
		}
	}
	list.onOpen = openSelected

	pickFiles := func() {
		dlg := &walk.FileDialog{Title: "공유할 파일 선택", Filter: "모든 파일 (*.*)|*.*"}
		if ok, _ := dlg.ShowOpenMultiple(mw); ok {
			addFiles(dlg.FilePaths)
		}
	}

	saveAs := func() {
		sel := filesOnly(list.selected())
		if len(sel) != 1 {
			setStatus("저장할 파일을 하나만 선택하세요.")
			return
		}
		dlg := &walk.FileDialog{Title: "다른 이름으로 저장", FilePath: sel[0].Name, Filter: "모든 파일 (*.*)|*.*"}
		if ok, _ := dlg.ShowSave(mw); ok {
			if err := copyFile(filepath.Join(shareDir, sel[0].Name), dlg.FilePath); err != nil {
				walk.MsgBox(mw, "파일공유", "저장하지 못했어요.\n"+err.Error(), walk.MsgBoxIconError)
			} else {
				setStatus("저장했어요 · " + dlg.FilePath)
			}
		}
	}

	reveal := func() {
		if sel := filesOnly(list.selected()); len(sel) > 0 {
			revealInFolder(filepath.Join(shareDir, sel[0].Name))
		} else {
			openFolder(shareDir)
		}
	}

	deleteSelected := func() {
		sel := list.selected()
		if len(sel) == 0 {
			setStatus("삭제할 파일을 목록에서 선택하세요.")
			return
		}
		name := sel[0].Name
		if sel[0].ClipID != "" {
			name = preview(sel[0].Text, 20)
		}
		msg := fmt.Sprintf("'%s'을(를) 삭제할까요?", name)
		if len(sel) > 1 {
			msg = fmt.Sprintf("선택한 항목 %d개를 삭제할까요?", len(sel))
		}
		if walk.MsgBox(mw, "삭제", msg, walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) != win.IDYES {
			return
		}
		for _, f := range sel {
			if f.ClipID != "" {
				deleteClip(f.ClipID)
			} else {
				os.Remove(filepath.Join(shareDir, f.Name))
			}
		}
		refresh(true)
	}

	paste := func() {
		res, err := readClipboard(mw.Handle())
		if err != nil {
			setStatus(err.Error())
			return
		}
		if len(res.files) > 0 {
			addFiles(res.files)
			return
		}
		refresh(true)
		list.sel = map[string]bool{}
		if res.isText && len(list.items) > 0 {
			list.sel[list.items[0].Name] = true
		}
		for _, p := range res.saved {
			list.sel[filepath.Base(p)] = true
		}
		list.offset = 0
		list.cw.Invalidate()
		if res.isText {
			setStatus("텍스트를 추가했어요. 다른 기기에서 [복사]를 누르면 바로 붙여넣을 수 있어요.")
		} else {
			setStatus("캡처 이미지를 추가했어요 · " + filepath.Base(res.saved[0]))
		}
	}

	// Ctrl+C: 선택한 텍스트/이미지 복사
	copyText := func() {
		sel := list.selected()
		if len(sel) != 1 || !canCopy(sel[0]) {
			setStatus("복사할 텍스트나 이미지를 하나 선택하세요.")
			return
		}
		copyItem(sel[0])
	}

	// [업데이트 확인]: 새 버전이 있으면 바로 받아서 바꾸고 다시 켬
	updating := false
	checkAndUpdate := func() {
		if updating {
			return
		}
		updating = true
		setStatus("업데이트 확인 중…")
		go func() {
			done := func(msg string) {
				mw.Synchronize(func() { updating = false; setStatus(msg) })
			}
			info, err := checkUpdate()
			if err != nil {
				done(err.Error())
				return
			}
			if info == nil {
				done("최신 버전이에요 (v" + appVersion + ")")
				return
			}
			last := time.Time{}
			err = downloadAndReplace(info, func(n, total int64) {
				if time.Since(last) < 200*time.Millisecond {
					return
				}
				last = time.Now()
				msg := fmt.Sprintf("v%s 받는 중… %s", info.Version, humanSize(n))
				if total > 0 {
					msg = fmt.Sprintf("v%s 받는 중… %d%%", info.Version, n*100/total)
				}
				mw.Synchronize(func() { setStatus(msg) })
			})
			if err != nil {
				done(err.Error())
				return
			}
			if err := restartUpdated(); err != nil {
				done("업데이트는 받았어요. 프로그램을 종료했다가 다시 켜 주세요.")
				return
			}
			mw.Synchronize(func() { setStatus("v" + info.Version + " 적용 중… 잠시 후 다시 켜져요") })
			quitApp()
		}()
	}

	confirmQuit := func() {
		if walk.MsgBox(mw, "파일공유 종료", "파일공유를 종료할까요?\n종료하면 다른 기기에서도 접속할 수 없어요.",
			walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) == win.IDYES {
			quitApp()
		}
	}

	list.onChange = func() {
		if n := len(list.selected()); n > 1 {
			setStatus(fmt.Sprintf("%d개 선택됨", n))
		} else if n == 1 {
			setStatus("더블클릭하면 기본 프로그램으로 열려요 · 오른쪽 클릭으로 더 많은 기능")
		}
	}

	bgBrush := SolidColorBrush{Color: colBg}
	cardBrush := SolidColorBrush{Color: colCard}
	f := func(size int, bold bool) Font { return Font{Family: uiFont, PointSize: size, Bold: bold} }

	err := MainWindow{
		AssignTo:    &mw,
		Title:       "파일공유",
		Icon:        appIcon,
		Size:        Size{Width: 760, Height: 620},
		MinSize:     Size{Width: 620, Height: 480},
		Background:  bgBrush,
		Font:        f(9, false),
		Layout:      VBox{Margins: Margins{Left: 18, Top: 14, Right: 18, Bottom: 10}, Spacing: 10},
		OnDropFiles: addFiles,
		Children: []Widget{
			// 머리말: 제목 + 상태 + 종료
			Composite{
				MaxSize: Size{Height: 30},
				Layout:  HBox{MarginsZero: true, Spacing: 8},
				Children: []Widget{
					Label{Text: "파일공유", Font: f(14, true), TextColor: colText},
					Label{Text: "같은 Wi-Fi 기기끼리 파일·텍스트 주고받기", TextColor: colMuted},
					HSpacer{},
					Label{Text: "● 실행 중", TextColor: colGreen},
					HSpacer{Size: 6},
					updateButton(checkAndUpdate),
					flatButton("종료", btnSecondary, colBg, confirmQuit),
				},
			},
			// 접속 카드
			Composite{
				Background: cardBrush,
				MaxSize:    Size{Height: 128},
				Layout:     HBox{Margins: Margins{Left: 16, Top: 14, Right: 16, Bottom: 14}, Spacing: 18},
				Children: []Widget{
					qrWidget(),
					Composite{
						Background: cardBrush,
						Layout:     VBox{MarginsZero: true, Spacing: 3},
						Children: []Widget{
							VSpacer{},
							Label{Text: "다른 기기에서 접속", TextColor: colMuted},
							Label{AssignTo: &addrLbl, Text: addrText(curIP), Font: f(15, true), TextColor: colAccent},
							Label{Text: "같은 Wi-Fi의 휴대폰·태블릿·PC 브라우저에서 주소를 열거나, 카메라로 QR을 찍으세요.", TextColor: colMuted},
							Composite{
								Background: cardBrush,
								Layout:     HBox{Margins: Margins{Top: 6}, Spacing: 6},
								Children: []Widget{
									flatButton("주소 복사", btnSecondary, colCard, func() {
										walk.Clipboard().SetText(lanURL())
										setStatus("주소를 복사했어요.")
									}),
									flatButton("저장 폴더 열기", btnSecondary, colCard, func() { openFolder(shareDir) }),
									HSpacer{},
								},
							},
							VSpacer{},
						},
					},
				},
			},
			// 도구 모음
			Composite{
				MaxSize: Size{Height: 30},
				Layout:  HBox{MarginsZero: true, Spacing: 6},
				Children: []Widget{
					Label{Text: "공유 목록", Font: f(11, true), TextColor: colText},
					Label{AssignTo: &countLbl, TextColor: colMuted},
					HSpacer{},
					flatButton("열기", btnSecondary, colBg, openSelected),
					flatButton("폴더에서 보기", btnSecondary, colBg, reveal),
					flatButton("다른 이름으로 저장", btnSecondary, colBg, saveAs),
					flatButton("삭제", btnDanger, colBg, deleteSelected),
					flatButton("＋ 파일 추가", btnPrimary, colBg, pickFiles),
				},
			},
			// 파일 목록 카드
			Composite{
				Background:    cardBrush,
				StretchFactor: 10,
				Layout:        VBox{Margins: Margins{Top: 4, Bottom: 4}},
				Children: []Widget{
					CustomWidget{
						AssignTo:            &list.cw,
						PaintMode:           PaintNoErase,
						InvalidatesOnResize: true,
						PaintPixels:         func(c *walk.Canvas, r walk.Rectangle) error { return list.paint(c, r) },
						OnMouseDown:         func(x, y int, b walk.MouseButton) { list.click(x, y, b) },
						OnMouseMove:         func(x, y int, b walk.MouseButton) { list.mouseMove(x, y, b) },
						OnMouseUp:           func(x, y int, b walk.MouseButton) { list.dragY = -1 },
						OnKeyDown: func(key walk.Key) {
							switch {
							case key == walk.KeyDelete:
								deleteSelected()
							case key == walk.KeyReturn:
								openSelected()
							case key == walk.KeyA && walk.ControlDown():
								for _, it := range list.items {
									list.sel[it.Name] = true
								}
								list.cw.Invalidate()
								list.onChange()
							}
						},
						ContextMenuItems: []MenuItem{
							Action{Text: "붙여넣기\tCtrl+V", OnTriggered: paste},
							Action{Text: "복사\tCtrl+C", OnTriggered: copyText},
							Separator{},
							Action{Text: "열기", OnTriggered: openSelected},
							Action{Text: "폴더에서 보기", OnTriggered: reveal},
							Action{Text: "다른 이름으로 저장...", OnTriggered: saveAs},
							Separator{},
							Action{Text: "삭제", OnTriggered: deleteSelected},
						},
					},
				},
			},
			Composite{
				MaxSize: Size{Height: 20},
				Layout:  HBox{MarginsZero: true},
				Children: []Widget{
					Label{AssignTo: &status, TextColor: colMuted,
						Text: "Ctrl+V로 캡처·텍스트·파일을 바로 추가해요 · [복사]를 누르면 바로 복사돼요 · 창을 닫아도 트레이에서 실행돼요"},
					HSpacer{},
					Label{Text: "v" + appVersion, TextColor: walk.RGB(156, 163, 175)},
				},
			},
		},
	}.Create()
	if err != nil {
		showError("화면을 만들지 못했어요: " + err.Error())
		select {}
	}
	setHandCursor(mw)

	// Ctrl+V = 붙여넣기 (창 어디에서든)
	pasteAction := walk.NewAction()
	pasteAction.SetShortcut(walk.Shortcut{Modifiers: walk.ModControl, Key: walk.KeyV})
	pasteAction.Triggered().Attach(paste)
	mw.ShortcutActions().Add(pasteAction)

	// Ctrl+C = 선택한 텍스트/이미지 복사
	copyAction := walk.NewAction()
	copyAction.SetShortcut(walk.Shortcut{Modifiers: walk.ModControl, Key: walk.KeyC})
	copyAction.Triggered().Attach(copyText)
	mw.ShortcutActions().Add(copyAction)
	list.cw.SetCursor(walk.CursorArrow())
	qrCW.SetCursor(walk.CursorArrow())
	list.cw.MouseWheel().Attach(func(x, y int, b walk.MouseButton) { list.wheel(walk.MouseWheelEventDelta(b)) })

	// 트레이 아이콘
	ni, _ = walk.NewNotifyIcon(mw)
	ni.SetIcon(appIcon)
	ni.SetToolTip("파일공유 실행 중\n" + addrText(curIP))
	ni.MouseDown().Attach(func(x, y int, button walk.MouseButton) {
		if button == walk.LeftButton {
			showWindow()
		}
	})
	addAction := func(text string, fn func()) *walk.Action {
		a := walk.NewAction()
		a.SetText(text)
		if fn != nil {
			a.Triggered().Attach(fn)
		}
		ni.ContextMenu().Actions().Add(a)
		return a
	}
	addAction("파일공유 열기", showWindow)
	addr := addAction("접속 주소: "+addrText(curIP), nil)
	addr.SetEnabled(false)
	addAction("저장 폴더 열기", func() { openFolder(shareDir) })
	if updateURL != "" {
		addAction("업데이트 확인", func() { showWindow(); checkAndUpdate() })
	}
	ni.ContextMenu().Actions().Add(walk.NewSeparatorAction())
	addAction("종료", quitApp)
	ni.SetVisible(true)

	// X 버튼 = 트레이로 숨기기
	hintShown := false
	mw.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		if quitting.Load() {
			return
		}
		*canceled = true
		mw.Hide()
		if !hintShown {
			hintShown = true
			ni.ShowInfo("파일공유가 계속 실행 중이에요", "오른쪽 아래 아이콘을 누르면 다시 열려요. 끄려면 아이콘을 오른쪽 클릭 → 종료")
		}
	})

	// 노트북을 들고 이동해서 IP가 바뀌면 주소·QR을 자동으로 새로 고침
	// (서버는 모든 네트워크에서 받도록 열려 있어서 다시 켤 필요 없음)
	go func() {
		seen := curIP
		for range time.Tick(3 * time.Second) {
			ip := localIP()
			if ip == seen {
				continue
			}
			seen = ip
			mw.Synchronize(func() {
				old := qrBmp
				qrBmp = nil
				if ip != "127.0.0.1" {
					qrBmp = makeQR(lanURL())
				}
				if old != nil {
					old.Dispose()
				}
				qrCW.Invalidate()
				addrLbl.SetText(addrText(ip))
				addr.SetText("접속 주소: " + addrText(ip))
				ni.SetToolTip("파일공유 실행 중\n" + addrText(ip))
				if ip == "127.0.0.1" {
					setStatus("네트워크 연결이 끊겼어요. Wi-Fi에 연결되면 주소가 자동으로 바뀌어요.")
				} else {
					setStatus("네트워크가 바뀌어서 접속 주소를 새로 고쳤어요 · " + lanURL())
				}
			})
		}
	}()

	if justUpdated {
		setStatus("v" + appVersion + " 업데이트를 마쳤어요.")
	}

	refresh(true)
	// 다른 기기에서 올린 파일 자동 반영
	go func() {
		for range time.Tick(2 * time.Second) {
			mw.Synchronize(func() { refresh(false) })
		}
	}()

	mw.Run()
	if ni != nil {
		ni.Dispose()
	}
	os.Exit(0)
}

// 업데이트 주소가 정해진 빌드에서만 [업데이트 확인] 버튼 표시
func updateButton(onClick func()) Widget {
	if updateURL == "" {
		return HSpacer{Size: 0}
	}
	return flatButton("업데이트 확인", btnSecondary, colBg, onClick)
}

// QR 코드 (정해진 크기로 직접 그림, 주소가 바뀌면 qrBmp를 바꾸고 다시 그림)
var (
	qrCW  *walk.CustomWidget
	qrBmp *walk.Bitmap
)

func makeQR(url string) *walk.Bitmap {
	q, err := qrcode.New(url, qrcode.Medium)
	if err != nil {
		return nil
	}
	q.DisableBorder = true
	b, _ := walk.NewBitmapFromImageForDPI(q.Image(240), 96)
	return b
}

// 화면에 보여줄 접속 주소 (네트워크가 없으면 안내 문구)
func addrText(ip string) string {
	if ip == "127.0.0.1" {
		return "네트워크 연결 없음"
	}
	return fmt.Sprintf("http://%s:%d", ip, port)
}

func qrWidget() Widget {
	return CustomWidget{
		AssignTo:            &qrCW,
		MinSize:             Size{Width: 100, Height: 100},
		MaxSize:             Size{Width: 100, Height: 100},
		InvalidatesOnResize: true,
		PaintMode:           PaintNoErase,
		PaintPixels: func(target *walk.Canvas, _ walk.Rectangle) error {
			return paintAtWindowDPI(qrCW, target, func(c *walk.Canvas, b walk.Rectangle) error {
				c.FillRectanglePixels(mustBrush(colCard), b)
				if qrBmp != nil {
					c.DrawImageStretchedPixels(qrBmp, b)
				}
				return nil
			})
		},
	}
}

// 직접 그린 버튼 위에서는 손가락 커서
func setHandCursor(c walk.Container) {
	children := c.Children()
	for i := 0; i < children.Len(); i++ {
		w := children.At(i)
		if cw, ok := w.(*walk.CustomWidget); ok {
			cw.SetCursor(walk.CursorHand())
		}
		if sub, ok := w.(walk.Container); ok {
			setHandCursor(sub)
		}
	}
}
