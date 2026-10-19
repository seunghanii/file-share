//go:build windows

// Ctrl+V 붙여넣기: 캡처 이미지(Win+Shift+S 등)나 탐색기에서 복사한 파일을 공유폴더에 저장
package main

import (
	"bytes"
	"errors"
	"image/png"
	"os"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/lxn/win"
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procGlobalSize   = kernel32.NewProc("GlobalSize")
	user32           = syscall.NewLazyDLL("user32.dll")
	procRegisterCF   = user32.NewProc("RegisterClipboardFormatW")
	procIsCFAvailble = user32.NewProc("IsClipboardFormatAvailable")
)

func cfAvailable(format uint32) bool {
	r, _, _ := procIsCFAvailble.Call(uintptr(format))
	return r != 0
}

func registeredFormat(name string) uint32 {
	p, _ := syscall.UTF16PtrFromString(name)
	r, _, _ := procRegisterCF.Call(uintptr(unsafe.Pointer(p)))
	return uint32(r)
}

// 클립보드 데이터를 바이트로 복사
func clipboardBytes(format uint32) []byte {
	h := win.GetClipboardData(format)
	if h == 0 {
		return nil
	}
	p := win.GlobalLock(win.HGLOBAL(h))
	if p == nil {
		return nil
	}
	defer win.GlobalUnlock(win.HGLOBAL(h))
	n, _, _ := procGlobalSize.Call(uintptr(h))
	if n == 0 {
		return nil
	}
	return append([]byte(nil), unsafe.Slice((*byte)(p), int(n))...)
}

// 붙여넣기 결과: 저장된 파일들 또는 복사된 원본 파일 경로들
type pasteResult struct {
	saved  []string // 새로 만든 파일 (캡처 이미지)
	files  []string // 탐색기에서 복사한 파일 경로
	isText bool     // 텍스트 클립으로 추가했는지
}

func readClipboard(hwnd win.HWND) (pasteResult, error) {
	var res pasteResult
	var ok bool
	for i := 0; i < 10; i++ { // 다른 프로그램이 잠깐 잡고 있을 수 있음
		if ok = win.OpenClipboard(hwnd); ok {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !ok {
		return res, errors.New("클립보드를 열 수 없어요")
	}
	defer win.CloseClipboard()

	// 1) 탐색기에서 복사한 파일
	if cfAvailable(win.CF_HDROP) {
		h := win.HDROP(win.GetClipboardData(win.CF_HDROP))
		n := win.DragQueryFile(h, 0xFFFFFFFF, nil, 0)
		for i := uint(0); i < n; i++ {
			l := win.DragQueryFile(h, i, nil, 0)
			buf := make([]uint16, l+1)
			win.DragQueryFile(h, i, &buf[0], l+1)
			res.files = append(res.files, syscall.UTF16ToString(buf))
		}
		if len(res.files) > 0 {
			return res, nil
		}
	}

	// 2) 복사한 텍스트 → 텍스트 클립 (파일 없이 바로 복사용)
	if cfAvailable(win.CF_UNICODETEXT) {
		if b := clipboardBytes(win.CF_UNICODETEXT); len(b) >= 2 {
			u := unsafe.Slice((*uint16)(unsafe.Pointer(&b[0])), len(b)/2)
			if text := syscall.UTF16ToString(u); strings.TrimSpace(text) != "" {
				if _, ok := addClip(text); !ok {
					return res, errors.New("텍스트가 너무 길어요")
				}
				res.isText = true
				return res, nil
			}
		}
	}

	// 3) 캡처 이미지: PNG 형식이 있으면 그대로, 없으면 DIB를 PNG로 변환
	var pngData []byte
	if f := registeredFormat("PNG"); f != 0 && cfAvailable(f) {
		pngData = clipboardBytes(f)
	}
	if pngData == nil && cfAvailable(win.CF_DIB) {
		img, err := decodeDIB(clipboardBytes(win.CF_DIB))
		if err != nil {
			return res, err
		}
		var b bytes.Buffer
		if err := png.Encode(&b, img); err != nil {
			return res, err
		}
		pngData = b.Bytes()
	}
	if pngData == nil {
		return res, errors.New("붙여넣을 내용이 없어요. 캡처(Win+Shift+S)하거나 텍스트·파일을 복사한 뒤 Ctrl+V 하세요")
	}
	name := "캡처 " + time.Now().Format("2006-01-02 15.04.05") + ".png"
	path := uniquePath(name)
	if err := os.WriteFile(path, pngData, 0o644); err != nil {
		return res, err
	}
	res.saved = append(res.saved, path)
	return res, nil
}

// ---------- 이미지를 클립보드로 복사 ----------

var (
	procGlobalAlloc      = kernel32.NewProc("GlobalAlloc")
	procGlobalFree       = kernel32.NewProc("GlobalFree")
	procSetClipboardData = user32.NewProc("SetClipboardData")
	procEmptyClipboard   = user32.NewProc("EmptyClipboard")
)

func putClipboard(format uint32, data []byte) bool {
	h, _, _ := procGlobalAlloc.Call(0x0002, uintptr(len(data))) // GMEM_MOVEABLE
	if h == 0 {
		return false
	}
	p := win.GlobalLock(win.HGLOBAL(h))
	if p == nil {
		procGlobalFree.Call(h)
		return false
	}
	copy(unsafe.Slice((*byte)(p), len(data)), data)
	win.GlobalUnlock(win.HGLOBAL(h))
	if r, _, _ := procSetClipboardData.Call(uintptr(format), h); r == 0 {
		procGlobalFree.Call(h)
		return false
	}
	return true
}

// 이미지 파일을 클립보드에 넣어서 다른 프로그램에 Ctrl+V로 붙여넣을 수 있게 함
func copyImageToClipboard(hwnd win.HWND, path string) error {
	img, err := loadImage(path)
	if err != nil {
		return err
	}
	var pngBuf bytes.Buffer
	png.Encode(&pngBuf, img)
	dib := encodeDIB(img)

	var ok bool
	for i := 0; i < 10; i++ {
		if ok = win.OpenClipboard(hwnd); ok {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !ok {
		return errors.New("클립보드를 열 수 없어요")
	}
	defer win.CloseClipboard()
	procEmptyClipboard.Call()
	if !putClipboard(win.CF_DIB, dib) {
		return errors.New("이미지를 복사하지 못했어요")
	}
	if f := registeredFormat("PNG"); f != 0 {
		putClipboard(f, pngBuf.Bytes()) // 투명도를 지원하는 프로그램용
	}
	return nil
}
