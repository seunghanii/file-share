package main

import (
	"encoding/binary"
	"testing"
)

// 2x2 DIB를 만들어 픽셀이 제자리에 오는지 확인
func makeDIB(bpp int, topDown bool, alpha byte) []byte {
	w, h := 2, 2
	stride := ((w*bpp + 31) / 32) * 4
	b := make([]byte, 40+stride*h)
	le := binary.LittleEndian
	le.PutUint32(b[0:], 40)
	le.PutUint32(b[4:], uint32(w))
	hh := int32(h)
	if topDown {
		hh = -hh
	}
	le.PutUint32(b[8:], uint32(hh))
	le.PutUint16(b[12:], 1)
	le.PutUint16(b[14:], uint16(bpp))
	// 위 줄: 빨강, 초록 / 아래 줄: 파랑, 흰색
	rows := [][][3]byte{{{255, 0, 0}, {0, 255, 0}}, {{0, 0, 255}, {255, 255, 255}}}
	for y := 0; y < h; y++ {
		sy := h - 1 - y
		if topDown {
			sy = y
		}
		for x := 0; x < w; x++ {
			c := rows[y][x]
			o := 40 + sy*stride + x*bpp/8
			b[o], b[o+1], b[o+2] = c[2], c[1], c[0]
			if bpp == 32 {
				b[o+3] = alpha
			}
		}
	}
	return b
}

func TestDecodeDIB(t *testing.T) {
	for _, tc := range []struct {
		bpp     int
		topDown bool
	}{{24, false}, {32, false}, {32, true}, {24, true}} {
		img, err := decodeDIB(makeDIB(tc.bpp, tc.topDown, 0))
		if err != nil {
			t.Fatal(err)
		}
		r, g, b, a := img.At(0, 0).RGBA()
		if r>>8 != 255 || g != 0 || b != 0 || a>>8 != 255 {
			t.Errorf("%v: top-left = %d %d %d %d, want opaque red", tc, r>>8, g>>8, b>>8, a>>8)
		}
		r, g, b, _ = img.At(0, 1).RGBA()
		if r != 0 || g != 0 || b>>8 != 255 {
			t.Errorf("%v: bottom-left = %d %d %d, want blue", tc, r>>8, g>>8, b>>8)
		}
	}
}

func TestEncodeDecodeDIB(t *testing.T) {
	src, err := decodeDIB(makeDIB(24, false, 0))
	if err != nil {
		t.Fatal(err)
	}
	back, err := decodeDIB(encodeDIB(src))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range [][2]int{{0, 0}, {1, 0}, {0, 1}, {1, 1}} {
		r1, g1, b1, _ := src.At(p[0], p[1]).RGBA()
		r2, g2, b2, _ := back.At(p[0], p[1]).RGBA()
		if r1 != r2 || g1 != g2 || b1 != b2 {
			t.Errorf("pixel %v differs after round trip", p)
		}
	}
}
