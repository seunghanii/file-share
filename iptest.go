//go:build iptest

package main

import (
	"os"
	"strings"
)

// 테스트 빌드 전용: C:\fakeip.txt 내용을 이 PC의 IP로 사용
func init() {
	localIPHook = func() string {
		b, err := os.ReadFile(`Z:\tmp\claude-0\fakeip.txt`)
		if err != nil {
			return "127.0.0.1"
		}
		return strings.TrimSpace(string(b))
	}
}
