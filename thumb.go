package main

// 이미지 미리보기(썸네일): 목록에서 PNG 배지 대신 실제 사진을 보여줌.
// 가운데를 정사각형으로 잘라 줄이고, 휴대폰 사진의 회전 정보(EXIF)를 반영합니다.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// 썸네일을 만들 수 있는 이미지 형식
func hasThumb(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".gif":
		return true
	}
	return false
}

type thumbKey struct {
	path  string
	mtime int64
	size  int
}

var (
	thumbMu    sync.Mutex
	thumbCache = map[thumbKey]image.Image{}
)

// path 이미지의 size×size 썸네일 (같은 파일이면 다시 만들지 않음)
func thumbnail(path string, size int) (image.Image, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	key := thumbKey{path, st.ModTime().UnixNano(), size}
	thumbMu.Lock()
	if t, ok := thumbCache[key]; ok {
		thumbMu.Unlock()
		return t, nil
	}
	thumbMu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	t := squareThumb(src, size, jpegOrientation(data))

	thumbMu.Lock()
	if len(thumbCache) > 500 {
		thumbCache = map[thumbKey]image.Image{}
	}
	thumbCache[key] = t
	thumbMu.Unlock()
	return t, nil
}

// 가운데 정사각형을 잘라 size×size로 줄임 (영역 평균이라 줄여도 깔끔함). 투명한 부분은 흰색.
func squareThumb(src image.Image, size, orient int) *image.RGBA {
	b := src.Bounds()
	side := b.Dx()
	if b.Dy() < side {
		side = b.Dy()
	}
	x0, y0 := b.Min.X+(b.Dx()-side)/2, b.Min.Y+(b.Dy()-side)/2

	// 정사각형 부분을 RGBA로 (흰 배경 위에)
	sq := image.NewRGBA(image.Rect(0, 0, side, side))
	draw.Draw(sq, sq.Bounds(), image.White, image.Point{}, draw.Src)
	draw.Draw(sq, sq.Bounds(), src, image.Point{x0, y0}, draw.Over)

	out := image.NewRGBA(image.Rect(0, 0, size, size))
	for ty := 0; ty < size; ty++ {
		sy0, sy1 := ty*side/size, (ty+1)*side/size
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for tx := 0; tx < size; tx++ {
			sx0, sx1 := tx*side/size, (tx+1)*side/size
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var r, g, bl, n int
			for y := sy0; y < sy1; y++ {
				i := sq.PixOffset(sx0, y)
				for x := sx0; x < sx1; x++ {
					r += int(sq.Pix[i])
					g += int(sq.Pix[i+1])
					bl += int(sq.Pix[i+2])
					i += 4
					n++
				}
			}
			ox, oy := orientXY(tx, ty, size, orient)
			out.SetRGBA(ox, oy, color.RGBA{uint8(r / n), uint8(g / n), uint8(bl / n), 255})
		}
	}
	return out
}

// EXIF 회전값(1~8)에 맞게 좌표 변환 (정사각형이라 크기는 그대로)
func orientXY(x, y, n, o int) (int, int) {
	m := n - 1
	switch o {
	case 2:
		return m - x, y
	case 3:
		return m - x, m - y
	case 4:
		return x, m - y
	case 5:
		return y, x
	case 6:
		return m - y, x
	case 7:
		return m - y, m - x
	case 8:
		return y, m - x
	}
	return x, y
}

// JPEG의 EXIF 회전값 (없으면 1)
func jpegOrientation(data []byte) int {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return 1
	}
	p := 2
	for p+4 <= len(data) && data[p] == 0xFF {
		marker := data[p+1]
		l := int(binary.BigEndian.Uint16(data[p+2:]))
		if marker == 0xDA || l < 2 || p+2+l > len(data) {
			break
		}
		seg := data[p+4 : p+2+l]
		if marker == 0xE1 && len(seg) > 14 && string(seg[:6]) == "Exif\x00\x00" {
			return tiffOrientation(seg[6:])
		}
		p += 2 + l
	}
	return 1
}

func tiffOrientation(t []byte) int {
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	off := int(bo.Uint32(t[4:]))
	if off+2 > len(t) {
		return 1
	}
	n := int(bo.Uint16(t[off:]))
	for i := 0; i < n; i++ {
		e := off + 2 + i*12
		if e+12 > len(t) {
			break
		}
		if bo.Uint16(t[e:]) == 0x0112 {
			if v := int(bo.Uint16(t[e+8:])); v >= 1 && v <= 8 {
				return v
			}
		}
	}
	return 1
}

// GET /api/thumb/{파일이름}: 목록용 작은 JPEG
func handleThumb(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/thumb/")
	p, ok := resolve(name)
	if !ok || !hasThumb(p) {
		http.NotFound(w, r)
		return
	}
	t, err := thumbnail(p, 128)
	if err != nil {
		http.Error(w, err.Error(), 415)
		return
	}
	var b bytes.Buffer
	jpeg.Encode(&b, t, &jpeg.Options{Quality: 82})
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "max-age=86400") // 주소에 수정 시각이 들어 있어서 오래 저장해도 됨
	w.Header().Set("Content-Length", fmt.Sprint(b.Len()))
	io.Copy(w, &b)
}
