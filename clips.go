package main

// 텍스트 클립: 파일로 만들지 않고 바로 복사할 수 있게 주고받는 텍스트.
// 공유폴더의 숨김 파일(.clips.json)에 보관해서 프로그램을 다시 켜도 남아 있습니다.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type clip struct {
	ID   string  `json:"id"`
	Text string  `json:"text"`
	Time float64 `json:"time"`
}

const (
	maxClipBytes = 256 * 1024
	maxClips     = 100
)

var (
	clipsMu sync.Mutex
	clips   []clip // 최신이 앞
)

func clipsPath() string { return filepath.Join(shareDir, ".clips.json") }

func loadClips() {
	clipsMu.Lock()
	defer clipsMu.Unlock()
	if b, err := os.ReadFile(clipsPath()); err == nil {
		json.Unmarshal(b, &clips)
	}
}

func saveClipsLocked() bool {
	b, _ := json.Marshal(clips)
	tmp := clipsPath() + ".part"
	return os.WriteFile(tmp, b, 0o644) == nil && os.Rename(tmp, clipsPath()) == nil
}

func listClips() []clip {
	clipsMu.Lock()
	defer clipsMu.Unlock()
	return append([]clip{}, clips...) // 비어 있어도 null이 아니라 [] 로 보냄
}

func addClip(text string) (clip, bool) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if strings.TrimSpace(text) == "" || len(text) > maxClipBytes || !utf8.ValidString(text) {
		return clip{}, false
	}
	id := make([]byte, 6)
	rand.Read(id)
	c := clip{ID: hex.EncodeToString(id), Text: text, Time: float64(time.Now().UnixNano()) / 1e9}
	clipsMu.Lock()
	defer clipsMu.Unlock()
	clips = append([]clip{c}, clips...)
	if len(clips) > maxClips {
		clips = clips[:maxClips]
	}
	saveClipsLocked()
	return c, true
}

func deleteClip(id string) bool {
	clipsMu.Lock()
	defer clipsMu.Unlock()
	for i, c := range clips {
		if c.ID == id {
			clips = append(clips[:i], clips[i+1:]...)
			saveClipsLocked()
			return true
		}
	}
	return false
}

// GET: 목록, POST: 텍스트 추가, DELETE /api/clips/{id}: 삭제
func handleClips(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, listClips())
	case http.MethodPost:
		b, err := io.ReadAll(io.LimitReader(r.Body, maxClipBytes+1))
		if err != nil {
			writeJSON(w, 400, map[string]any{"ok": false})
			return
		}
		c, ok := addClip(string(b))
		if !ok {
			writeJSON(w, 400, map[string]any{"ok": false, "error": "보낼 텍스트가 없거나 너무 길어요"})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "clip": c})
	case http.MethodDelete:
		id := strings.TrimPrefix(r.URL.Path, "/api/clips/")
		writeJSON(w, 200, map[string]any{"ok": deleteClip(id)})
	default:
		http.Error(w, "method not allowed", 405)
	}
}
