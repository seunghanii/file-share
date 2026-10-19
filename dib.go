package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
)

// 클립보드로 복사할 수 있는 이미지 형식
func isCopyableImage(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".gif":
		return true
	}
	return false
}

func loadImage(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, errors.New("이 이미지 형식은 복사할 수 없어요")
	}
	return img, nil
}

// image → CF_DIB (32비트, 아래에서 위로)
func encodeDIB(img image.Image) []byte {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	out := make([]byte, 40+w*h*4)
	le := binary.LittleEndian
	le.PutUint32(out[0:], 40)
	le.PutUint32(out[4:], uint32(w))
	le.PutUint32(out[8:], uint32(h))
	le.PutUint16(out[12:], 1)
	le.PutUint16(out[14:], 32)
	le.PutUint32(out[20:], uint32(w*h*4))
	px := out[40:]
	for y := 0; y < h; y++ {
		row := px[(h-1-y)*w*4:]
		for x := 0; x < w; x++ {
			c := color.NRGBAModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA)
			// 투명한 부분은 흰 배경으로 (대부분의 프로그램이 DIB 알파를 무시함)
			a := int(c.A)
			mix := func(v uint8) byte { return byte((int(v)*a + 255*(255-a)) / 255) }
			row[x*4], row[x*4+1], row[x*4+2], row[x*4+3] = mix(c.B), mix(c.G), mix(c.R), 255
		}
	}
	return out
}

// CF_DIB (BITMAPINFOHEADER + 픽셀) → image
func decodeDIB(b []byte) (image.Image, error) {
	if len(b) < 40 {
		return nil, errors.New("이미지 형식을 읽을 수 없어요")
	}
	le := binary.LittleEndian
	hdrSize := int(le.Uint32(b[0:]))
	w := int(int32(le.Uint32(b[4:])))
	h := int(int32(le.Uint32(b[8:])))
	bpp := int(le.Uint16(b[14:]))
	comp := le.Uint32(b[16:])
	clrUsed := int(le.Uint32(b[32:]))
	topDown := h < 0
	if topDown {
		h = -h
	}
	if w <= 0 || h <= 0 || w > 30000 || h > 30000 {
		return nil, errors.New("이미지 크기가 올바르지 않아요")
	}
	off := hdrSize
	if comp == 3 && hdrSize == 40 { // BI_BITFIELDS: 색 마스크 12바이트
		off += 12
	}
	var palette []color.RGBA
	if bpp <= 8 {
		n := clrUsed
		if n == 0 {
			n = 1 << bpp
		}
		for i := 0; i < n && off+4 <= len(b); i++ {
			palette = append(palette, color.RGBA{b[off+2], b[off+1], b[off], 255})
			off += 4
		}
	}
	if comp != 0 && comp != 3 {
		return nil, fmt.Errorf("지원하지 않는 이미지 압축 형식이에요 (%d)", comp)
	}
	stride := ((w*bpp + 31) / 32) * 4
	if off+stride*h > len(b) {
		return nil, errors.New("이미지 데이터가 잘렸어요")
	}
	px := b[off:]

	// 32비트인데 알파가 전부 0이면 불투명으로 취급
	opaque := true
	if bpp == 32 {
		for y := 0; y < h && opaque; y++ {
			row := px[y*stride:]
			for x := 0; x < w; x++ {
				if row[x*4+3] != 0 {
					opaque = false
					break
				}
			}
		}
	}

	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		sy := h - 1 - y
		if topDown {
			sy = y
		}
		row := px[sy*stride:]
		for x := 0; x < w; x++ {
			var c color.NRGBA
			switch bpp {
			case 32:
				a := row[x*4+3]
				if opaque {
					a = 255
				}
				c = color.NRGBA{row[x*4+2], row[x*4+1], row[x*4], a}
			case 24:
				c = color.NRGBA{row[x*3+2], row[x*3+1], row[x*3], 255}
			case 8, 4, 1:
				bit := x * bpp
				v := int(row[bit/8]>>(8-bpp-bit%8)) & (1<<bpp - 1)
				if v < len(palette) {
					p := palette[v]
					c = color.NRGBA{p.R, p.G, p.B, 255}
				}
			default:
				return nil, fmt.Errorf("지원하지 않는 색 깊이예요 (%d비트)", bpp)
			}
			img.SetNRGBA(x, y, c)
		}
	}
	return img, nil
}
