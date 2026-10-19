//go:build !windows

package main

import (
	"fmt"
	"os"
)

// 윈도우가 아닌 환경(개발/테스트용): 창 없이 서버만 실행
func runGUI()            { select {} }
func showWindow()        {}
func quitApp()           { os.Exit(0) }
func showError(m string) { fmt.Println(m) }
