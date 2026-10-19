package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
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

// 클립보드에 넣을 이미지 준비 (오래 걸릴 수 있어서 화면 스레드 밖에서 호출)
// dib: CF_DIB 데이터, pngRaw: 원본이 PNG면 파일 그대로 (다시 압축하지 않음)
func prepareClipboardImage(path string) (dib, pngRaw []byte, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, nil, errors.New("이 이미지 형식은 복사할 수 없어요")
	}
	orient := 1
	if format == "jpeg" {
		orient = jpegOrientation(data) // 휴대폰 사진의 회전 정보 반영
	}
	rgba := flattenRGBA(img)
	if orient != 1 {
		rgba = orientRGBA(rgba, orient)
	}
	if format == "png" {
		pngRaw = data
	}
	return encodeDIB(rgba), pngRaw, nil
}

// 흰 배경 위에 그린 RGBA (draw 패키지의 빠른 변환 사용)
func flattenRGBA(img image.Image) *image.RGBA {
	b := img.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	if _, opaque := img.(*image.YCbCr); !opaque {
		draw.Draw(out, out.Bounds(), image.White, image.Point{}, draw.Src)
		draw.Draw(out, out.Bounds(), img, b.Min, draw.Over)
	} else {
		draw.Draw(out, out.Bounds(), img, b.Min, draw.Src)
	}
	return out
}

// EXIF 회전값(2~8)대로 돌리거나 뒤집은 새 이미지
func orientRGBA(src *image.RGBA, o int) *image.RGBA {
	w, h := src.Rect.Dx(), src.Rect.Dy()
	ow, oh := w, h
	if o >= 5 {
		ow, oh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, ow, oh))
	for y := 0; y < h; y++ {
		si := y * src.Stride
		for x := 0; x < w; x++ {
			var dx, dy int
			switch o {
			case 2:
				dx, dy = w-1-x, y
			case 3:
				dx, dy = w-1-x, h-1-y
			case 4:
				dx, dy = x, h-1-y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = h-1-y, x
			case 7:
				dx, dy = h-1-y, w-1-x
			case 8:
				dx, dy = y, w-1-x
			default:
				dx, dy = x, y
			}
			copy(dst.Pix[dy*dst.Stride+dx*4:dy*dst.Stride+dx*4+4], src.Pix[si+x*4:si+x*4+4])
		}
	}
	return dst
}

// RGBA → CF_DIB (32비트, 아래에서 위로, 불투명)
func encodeDIB(img *image.RGBA) []byte {
	w, h := img.Rect.Dx(), img.Rect.Dy()
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
		src := img.Pix[y*img.Stride : y*img.Stride+w*4]
		row := px[(h-1-y)*w*4 : (h-y)*w*4]
		for i := 0; i < len(src); i += 4 {
			row[i], row[i+1], row[i+2], row[i+3] = src[i+2], src[i+1], src[i], 255
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
