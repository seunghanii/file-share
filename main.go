// 파일공유 서버 (윈도우 PC <-> 같은 Wi-Fi의 다른 기기)
//
// 더블클릭하면 서버와 프로그램 창이 켜집니다.
// 같은 Wi-Fi의 휴대폰·태블릿·다른 PC는 화면에 표시된 주소(또는 QR)로 접속합니다.
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

//go:embed index.html
var indexHTML []byte

//go:embed icon.png
var iconPNG []byte

const appID = "fileshare-ipad-pc"

var appVersion = "1.15" // 빌드할 때 -X main.appVersion 으로 바꿀 수 있음

var (
	shareDir string
	port     int
	badChars = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)
)

func init() {
	for ext, typ := range map[string]string{
		".heic": "image/heic", ".heif": "image/heif", ".webp": "image/webp",
		".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
		".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		".xls":  "application/vnd.ms-excel", ".ppt": "application/vnd.ms-powerpoint",
		".doc": "application/msword", ".hwp": "application/x-hwp", ".pdf": "application/pdf",
	} {
		mime.AddExtensionType(ext, typ)
	}
}

// ---------- 유틸 ----------

var localIPHook func() string // 테스트용 (iptest 빌드 태그)

func localIP() string {
	if localIPHook != nil {
		return localIPHook()
	}
	conn, err := net.Dial("udp", "8.8.8.8:80") // 실제 전송 없음, 경로 확인용
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

func lanURL() string { return fmt.Sprintf("http://%s:%d", localIP(), port) }

func safeName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Trim(badChars.ReplaceAllString(name, "_"), " .")
	if name == "" {
		return "file"
	}
	return name
}

func uniquePath(name string) string {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	p := filepath.Join(shareDir, name)
	for i := 1; ; i++ {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			return p
		}
		p = filepath.Join(shareDir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
	}
}

func resolve(name string) (string, bool) {
	p := filepath.Join(shareDir, safeName(name))
	st, err := os.Stat(p)
	return p, err == nil && st.Mode().IsRegular()
}

func isLocal(r *http.Request) bool {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func openURL(u string) {
	switch runtime.GOOS {
	case "windows":
		exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	case "darwin":
		exec.Command("open", u).Start()
	default:
		exec.Command("xdg-open", u).Start()
	}
}

func openFolder(p string) {
	if runtime.GOOS == "windows" {
		exec.Command("explorer", p).Start()
	} else {
		openURL(p)
	}
}

// ---------- 핸들러 ----------

type fileInfo struct {
	Name   string  `json:"name"`
	Size   int64   `json:"size"`
	Mtime  float64 `json:"mtime"`
	Text   string  `json:"-"` // PC 목록에서 텍스트 클립일 때 내용
	ClipID string  `json:"-"` // PC 목록에서 텍스트 클립이면 클립 ID
}

func readFiles() []fileInfo {
	entries, _ := os.ReadDir(shareDir)
	files := []fileInfo{}
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".part") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fileInfo{Name: e.Name(), Size: info.Size(), Mtime: float64(info.ModTime().UnixNano()) / 1e9})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Mtime > files[j].Mtime })
	return files
}

// 여러 줄 텍스트를 한 줄 미리보기로
func preview(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

func handleList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, readFiles())
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	raw, _ := url.QueryUnescape(r.Header.Get("X-Filename"))
	final := uniquePath(safeName(raw))
	tmp := final + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	n, err := io.Copy(f, r.Body)
	f.Close()
	if err == nil && r.ContentLength >= 0 && n != r.ContentLength {
		err = fmt.Errorf("업로드가 중간에 끊겼습니다")
	}
	if err == nil {
		err = os.Rename(tmp, final)
	}
	if err != nil {
		os.Remove(tmp)
		writeJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "name": filepath.Base(final)})
}

func handleFile(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/files/")
	p, ok := resolve(name)
	if !ok {
		http.Error(w, "파일이 없습니다.", 404)
		return
	}
	switch r.Method {
	case http.MethodDelete:
		if err := os.Remove(p); err != nil {
			writeJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	case http.MethodGet, http.MethodHead:
		fname := filepath.Base(p)
		disp := "inline"
		if r.URL.Query().Has("dl") {
			disp = "attachment"
		}
		w.Header().Set("Content-Disposition", contentDisposition(disp, fname))
		if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(fname))); t != "" {
			w.Header().Set("Content-Type", t)
		}
		http.ServeFile(w, r, p) // 이어받기(Range) 지원
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// 받은 파일이 원래 이름 그대로 저장되도록: filename*에는 UTF-8 이름(RFC 5987),
// filename에도 원래 이름을 그대로(공백·한글 포함) 넣음. 브라우저 PDF 뷰어처럼 filename만 보는 경우를 위해
// (예전에는 여기에 %20 같은 인코딩된 이름이 들어가서 그대로 저장되는 문제가 있었음)
func contentDisposition(disp, name string) string {
	var plain, ext strings.Builder
	for _, c := range name {
		switch {
		case c == '"' || c == '\\':
			plain.WriteByte('_')
		case c < 0x20 || c == 0x7f:
			plain.WriteByte('_')
		default:
			plain.WriteRune(c)
		}
	}
	for _, b := range []byte(name) {
		if b < 0x80 && (b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.IndexByte("!#$&+-.^_`|~", b) >= 0) {
			ext.WriteByte(b)
		} else {
			fmt.Fprintf(&ext, "%%%02X", b)
		}
	}
	return fmt.Sprintf(`%s; filename="%s"; filename*=UTF-8''%s`, disp, plain.String(), ext.String())
}

func handleInfo(w http.ResponseWriter, r *http.Request) {
	info := map[string]any{"app": appID, "version": appVersion, "local": isLocal(r), "url": lanURL(), "httpsUrl": httpsURL(), "needPin": !authorized(r)}
	w.Header().Set("Access-Control-Allow-Origin", "*") // http 페이지가 보안 주소를 쓸 수 있는지 확인할 때 필요
	if isLocal(r) {
		info["folder"] = shareDir
	}
	writeJSON(w, 200, info)
}

func handleQR(w http.ResponseWriter, r *http.Request) {
	png, err := qrcode.Encode(lanURL(), qrcode.Medium, 320)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(png)
}

// PC(이 컴퓨터)에서만 쓸 수 있는 기능
func localOnly(fn func()) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !isLocal(r) {
			http.Error(w, "forbidden", 403)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
		fn()
	}
}

// ---------- 시작 ----------

// 같은 프로그램이 이미 이 포트에서 실행 중인지 확인
func runningInstance(p int) bool {
	c := http.Client{Timeout: 700 * time.Millisecond}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/api/info", p))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var v map[string]any
	json.NewDecoder(resp.Body).Decode(&v)
	return v["app"] == appID
}

// 공유 폴더: exe 옆 '공유폴더' (쓸 수 없는 위치면 문서\파일공유)
func pickShareDir() string {
	var dirs []string
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Join(filepath.Dir(exe), "공유폴더"))
	}
	if d := documentsDir(); d != "" {
		dirs = append(dirs, filepath.Join(d, "파일공유"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, "Documents", "파일공유"))
	}
	for _, d := range dirs {
		if writable(d) {
			return d
		}
	}
	return os.TempDir()
}

func writable(d string) bool {
	if os.MkdirAll(d, 0o755) != nil {
		return false
	}
	f, err := os.CreateTemp(d, ".w")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}

func main() {
	afterUpdateStartup()
	shareDir = pickShareDir()
	loadClips()
	loadPIN()

	var ln net.Listener
	for p := 8000; p <= 8010; p++ {
		if runningInstance(p) { // 두 번 실행하면 이미 켜진 창만 다시 띄움
			http.Post(fmt.Sprintf("http://127.0.0.1:%d/api/show", p), "text/plain", nil)
			return
		}
		if l, err := net.Listen("tcp", fmt.Sprintf(":%d", p)); err == nil {
			ln, port = l, p
			break
		}
	}
	if ln == nil {
		showError("사용 가능한 포트(8000~8010)가 없습니다.")
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(indexHTML)
	})
	// 파일·텍스트를 다루는 기능은 접속 PIN 필요 (이 PC에서는 묻지 않음)
	mux.HandleFunc("/api/files", requirePIN(handleList))
	mux.HandleFunc("/api/upload", requirePIN(handleUpload))
	mux.HandleFunc("/api/clips", requirePIN(handleClips))
	mux.HandleFunc("/api/clips/", requirePIN(handleClips))
	mux.HandleFunc("/api/thumb/", requirePIN(handleThumb))
	mux.HandleFunc("/api/login", handleLogin)
	mux.HandleFunc("/api/info", handleInfo)
	mux.HandleFunc("/api/qr.png", requirePIN(handleQR))
	mux.HandleFunc("/api/open-folder", localOnly(func() { openFolder(shareDir) }))
	mux.HandleFunc("/api/show", localOnly(showWindow))
	mux.HandleFunc("/api/quit", localOnly(func() {
		go func() { time.Sleep(300 * time.Millisecond); quitApp() }()
	}))
	mux.HandleFunc("/files/", requirePIN(handleFile))
	mux.HandleFunc("/ca.crt", handleCACert)

	fmt.Printf("파일공유 서버 실행 중\n  이 PC: http://localhost:%d\n  다른 기기: %s\n  저장 폴더: %s\n", port, lanURL(), shareDir)
	go http.Serve(ln, mux)
	startHTTPS(mux) // 사진 바로 복사용 보안 주소 (https://IP:8443)

	runGUI() // 윈도우: 프로그램 창 + 트레이 아이콘 (gui_windows.go)
}
