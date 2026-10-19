//go:build windows

package main

import (
	"syscall"

	"github.com/lxn/win"
)

// 실제 '문서' 폴더 (OneDrive로 옮겨진 경우도 포함)
func documentsDir() string {
	buf := make([]uint16, win.MAX_PATH)
	if win.SHGetSpecialFolderPath(0, &buf[0], win.CSIDL_PERSONAL, false) {
		return syscall.UTF16ToString(buf)
	}
	return ""
}
