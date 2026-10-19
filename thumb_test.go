package main

import (
	"image"
	"image/color"
	"testing"
)

func TestSquareThumbOrientation(t *testing.T) {
	// 4x2 이미지: 왼쪽 절반 빨강, 오른쪽 절반 파랑 → 가운데 2x2를 잘라 2x2로
	src := image.NewRGBA(image.Rect(0, 0, 4, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 4; x++ {
			c := color.RGBA{255, 0, 0, 255}
			if x >= 2 {
				c = color.RGBA{0, 0, 255, 255}
			}
			src.SetRGBA(x, y, c)
		}
	}
	th := squareThumb(src, 2, 1)
	if th.RGBAAt(0, 0).R != 255 || th.RGBAAt(1, 0).B != 255 {
		t.Fatalf("crop wrong: %v %v", th.RGBAAt(0, 0), th.RGBAAt(1, 0))
	}
	// 6 = 시계 방향 90도: 왼쪽(빨강)이 위로
	th = squareThumb(src, 2, 6)
	if th.RGBAAt(0, 0).R != 255 || th.RGBAAt(1, 0).R != 255 || th.RGBAAt(0, 1).B != 255 {
		t.Fatalf("rotate wrong: %v %v %v", th.RGBAAt(0, 0), th.RGBAAt(1, 0), th.RGBAAt(0, 1))
	}
}

func TestJpegOrientation(t *testing.T) {
	exif := []byte("Exif\x00\x00II*\x00\x08\x00\x00\x00\x01\x00\x12\x01\x03\x00\x01\x00\x00\x00\x06\x00\x00\x00\x00\x00\x00\x00")
	seg := append([]byte{0xFF, 0xE1, 0, byte(len(exif) + 2)}, exif...)
	data := append(append([]byte{0xFF, 0xD8}, seg...), 0xFF, 0xDA, 0, 2)
	if o := jpegOrientation(data); o != 6 {
		t.Fatalf("got %d", o)
	}
	if o := jpegOrientation([]byte{1, 2, 3}); o != 1 {
		t.Fatalf("got %d", o)
	}
}
