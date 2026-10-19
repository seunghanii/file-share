package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPIN(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	loadPIN()
	h := requirePIN(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	req := func(method, path, body, cookie, from string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.RemoteAddr = from + ":5000"
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: pinCookie, Value: cookie})
		}
		w := httptest.NewRecorder()
		if path == "/api/login" {
			handleLogin(w, r)
		} else {
			h(w, r)
		}
		return w
	}
	if w := req("GET", "/api/files", "", "", "127.0.0.1"); w.Code != 200 {
		t.Fatal("이 PC는 PIN 없이 돼야 함", w.Code)
	}
	if w := req("GET", "/api/files", "", "", "192.168.0.5"); w.Code != 401 {
		t.Fatal("다른 기기는 401", w.Code)
	}
	if w := req("POST", "/api/login", `{"pin":"xxxx"}`, "", "192.168.0.5"); w.Code != 403 {
		t.Fatal("틀린 PIN 403", w.Code)
	}
	w := req("POST", "/api/login", `{"pin":"`+currentPIN()+`"}`, "", "192.168.0.5")
	if w.Code != 200 || len(w.Result().Cookies()) != 1 {
		t.Fatal("맞는 PIN", w.Code)
	}
	tok := w.Result().Cookies()[0].Value
	if w := req("GET", "/api/files", "", tok, "192.168.0.5"); w.Code != 200 {
		t.Fatal("쿠키로 통과", w.Code)
	}
	// 다시 불러와도 같은 PIN·쿠키 유효
	old := currentPIN()
	loadPIN()
	if currentPIN() != old || req("GET", "/api/files", "", tok, "192.168.0.5").Code != 200 {
		t.Fatal("재시작 후 유지 안 됨")
	}
	// PIN을 바꾸면 이전 쿠키 무효
	if changePIN() == old || req("GET", "/api/files", "", tok, "192.168.0.5").Code != 401 {
		t.Fatal("PIN 변경 후 이전 쿠키가 통함")
	}
	// 5번 틀리면 잠금 (맞는 PIN도 잠시 거절)
	for i := 0; i < 5; i++ {
		req("POST", "/api/login", `{"pin":"zzzz"}`, "", "192.168.0.9")
	}
	if w := req("POST", "/api/login", `{"pin":"`+currentPIN()+`"}`, "", "192.168.0.9"); w.Code != 429 {
		t.Fatal("잠금 안 됨", w.Code)
	}
}
