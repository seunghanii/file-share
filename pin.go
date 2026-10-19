package main

// 접속 PIN: 같은 네트워크의 다른 사람이 주소만 알고 들어오지 못하게,
// 기기마다 처음 한 번 4자리 번호를 입력하게 합니다. 입력하면 쿠키로 기억합니다.
// 이 PC에서 여는 화면은 PIN을 묻지 않습니다.
// PIN을 바꾸면 이전에 입력한 기기도 모두 다시 입력해야 합니다.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const pinCookie = "fs_pin"

var (
	pinMu     sync.Mutex
	pinCode   string
	pinSecret []byte

	failMu sync.Mutex
	fails  = map[string]*failInfo{}
)

type failInfo struct {
	count int
	until time.Time
}

type pinFile struct {
	PIN    string `json:"pin"`
	Secret string `json:"secret"`
}

func pinPath() string { return filepath.Join(certDir(), "pin.json") }

func randomPIN() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(10000))
	return fmt.Sprintf("%04d", n.Int64())
}

// PIN을 불러오거나 처음이면 새로 만듦
func loadPIN() {
	pinMu.Lock()
	defer pinMu.Unlock()
	var pf pinFile
	if b, err := os.ReadFile(pinPath()); err == nil && json.Unmarshal(b, &pf) == nil && len(pf.PIN) == 4 {
		if s, err := hex.DecodeString(pf.Secret); err == nil && len(s) >= 16 {
			pinCode, pinSecret = pf.PIN, s
			return
		}
	}
	setPINLocked(randomPIN())
}

func setPINLocked(code string) {
	s := make([]byte, 32)
	rand.Read(s) // 비밀값도 새로 만들어서 이전 기기의 쿠키가 더 이상 통하지 않게 함
	pinCode, pinSecret = code, s
	b, _ := json.Marshal(pinFile{PIN: code, Secret: hex.EncodeToString(s)})
	os.MkdirAll(certDir(), 0o700)
	os.WriteFile(pinPath(), b, 0o600)
}

func currentPIN() string {
	pinMu.Lock()
	defer pinMu.Unlock()
	return pinCode
}

// 새 PIN으로 바꾸기 (PC 창의 [PIN 바꾸기])
func changePIN() string {
	pinMu.Lock()
	defer pinMu.Unlock()
	code := randomPIN()
	for code == pinCode {
		code = randomPIN()
	}
	setPINLocked(code)
	return code
}

func pinToken() string {
	pinMu.Lock()
	defer pinMu.Unlock()
	m := hmac.New(sha256.New, pinSecret)
	m.Write([]byte("fileshare:" + pinCode))
	return hex.EncodeToString(m.Sum(nil))
}

func authorized(r *http.Request) bool {
	if isLocal(r) {
		return true
	}
	c, err := r.Cookie(pinCookie)
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(pinToken())) == 1
}

// PIN이 필요한 기능을 감쌈
func requirePIN(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			writeJSON(w, 401, map[string]any{"ok": false, "needPin": true})
			return
		}
		h(w, r)
	}
}

func clientIP(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

// POST /api/login  {"pin":"1234"}  — 5번 틀리면 1분 동안 잠금
func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	ip := clientIP(r)
	failMu.Lock()
	f := fails[ip]
	if f != nil && time.Now().Before(f.until) {
		wait := int(time.Until(f.until).Seconds()) + 1
		failMu.Unlock()
		writeJSON(w, 429, map[string]any{"ok": false, "error": fmt.Sprintf("너무 많이 틀렸어요. %d초 뒤에 다시 해 주세요", wait)})
		return
	}
	failMu.Unlock()

	var req struct {
		PIN string `json:"pin"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&req)
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(req.PIN)), []byte(currentPIN())) != 1 {
		failMu.Lock()
		if f == nil {
			f = &failInfo{}
			fails[ip] = f
		}
		f.count++
		if f.count >= 5 {
			f.count, f.until = 0, time.Now().Add(time.Minute)
		}
		failMu.Unlock()
		writeJSON(w, 403, map[string]any{"ok": false, "error": "PIN이 맞지 않아요"})
		return
	}
	failMu.Lock()
	delete(fails, ip)
	failMu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: pinCookie, Value: pinToken(), Path: "/",
		MaxAge: 400 * 24 * 3600, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}
