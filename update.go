package main

// 업데이트: [업데이트 확인]을 누르면 업데이트 주소의 version.json을 읽고,
// 새 버전이 있으면 exe를 내려받아 지금 exe와 바꾼 뒤 프로그램을 다시 켭니다.
//
// version.json 예: {"version":"1.10","url":"파일공유.exe","sha256":"…"}
// url은 version.json 기준 상대 경로도 됩니다. 업데이트 주소는 http(s) 주소나
// 회사 공유 폴더 경로(\\서버\폴더\version.json) 모두 가능합니다.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// 새 버전 정보를 받는 곳 (GitHub 저장소 seunghanii/file-share의 release 폴더).
// 테스트할 때는 -ldflags "-X main.updateURL=…" 로 바꿀 수 있음
var updateURL = "https://raw.githubusercontent.com/seunghanii/file-share/main/release/version.json"

type updateInfo struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
}

// "1.10" > "1.9" 처럼 숫자로 비교
func newerVersion(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x > y
		}
	}
	return false
}

func isHTTP(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

var (
	updateClient   = &http.Client{Timeout: 10 * time.Minute}
	manifestClient = &http.Client{Timeout: 15 * time.Second} // version.json은 작아서 짧게
)

// 주소(http 또는 파일 경로)를 열기
func openSource(src string) (io.ReadCloser, int64, error) { return openWith(updateClient, src) }

func openWith(client *http.Client, src string) (io.ReadCloser, int64, error) {
	if !isHTTP(src) {
		f, err := os.Open(src)
		if err != nil {
			return nil, 0, err
		}
		st, _ := f.Stat()
		return f, st.Size(), nil
	}
	req, _ := http.NewRequest("GET", src, nil)
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, 0, fmt.Errorf("서버 응답 %d", resp.StatusCode)
	}
	return resp.Body, resp.ContentLength, nil
}

// version.json 기준으로 상대 주소 풀기
func resolveRef(base, ref string) string {
	if isHTTP(ref) || filepath.IsAbs(ref) || strings.HasPrefix(ref, `\\`) {
		return ref
	}
	if isHTTP(base) {
		b, err := url.Parse(base)
		if err != nil {
			return ref
		}
		r, err := url.Parse(ref)
		if err != nil {
			return ref
		}
		return b.ResolveReference(r).String()
	}
	return filepath.Join(filepath.Dir(base), ref)
}

// 최신 버전 정보 (지금보다 새것이 없으면 nil)
func checkUpdate() (*updateInfo, error) {
	if updateURL == "" {
		return nil, errors.New("업데이트 주소가 설정되지 않았어요")
	}
	src := updateURL
	if isHTTP(src) {
		src += fmt.Sprintf("?t=%d", time.Now().Unix()) // 캐시된 옛 정보 방지
	}
	rc, _, err := openWith(manifestClient, src)
	if err != nil {
		return nil, fmt.Errorf("업데이트 정보를 가져오지 못했어요 (%v)", err)
	}
	defer rc.Close()
	var info updateInfo
	if err := json.NewDecoder(io.LimitReader(rc, 64*1024)).Decode(&info); err != nil || info.Version == "" || info.URL == "" {
		return nil, errors.New("업데이트 정보가 올바르지 않아요")
	}
	if !newerVersion(info.Version, appVersion) {
		return nil, nil
	}
	info.URL = resolveRef(updateURL, info.URL)
	return &info, nil
}

// 새 exe를 내려받아 지금 exe와 바꿈. progress(받은 바이트, 전체 바이트)
func downloadAndReplace(info *updateInfo, progress func(done, total int64)) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	newPath, oldPath := exe+".new", exe+".old"

	rc, total, err := openSource(info.URL)
	if err != nil {
		return fmt.Errorf("새 버전을 받지 못했어요 (%v)", err)
	}
	defer rc.Close()
	f, err := os.Create(newPath)
	if err != nil {
		return fmt.Errorf("새 버전을 저장하지 못했어요 (%v)", err)
	}
	h := sha256.New()
	var done int64
	buf := make([]byte, 256*1024)
	var head []byte
	for {
		n, rerr := rc.Read(buf)
		if n > 0 {
			if len(head) < 2 {
				head = append(head, buf[:n]...)
			}
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				os.Remove(newPath)
				return werr
			}
			h.Write(buf[:n])
			done += int64(n)
			if progress != nil {
				progress(done, total)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			os.Remove(newPath)
			return fmt.Errorf("받는 중에 끊겼어요 (%v)", rerr)
		}
	}
	f.Close()

	// 받은 파일 확인: 크기, 실행 파일 형식, 체크섬
	if (total > 0 && done != total) || done < 1024*1024 || len(head) < 2 || string(head[:2]) != "MZ" {
		os.Remove(newPath)
		return errors.New("받은 파일이 올바르지 않아요")
	}
	if info.SHA256 != "" && !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), info.SHA256) {
		os.Remove(newPath)
		return errors.New("받은 파일이 손상됐어요 (체크섬 불일치)")
	}

	// 실행 중인 exe는 지울 수 없지만 이름은 바꿀 수 있음 → 옛 파일은 .old로
	os.Remove(oldPath)
	if err := os.Rename(exe, oldPath); err != nil {
		os.Remove(newPath)
		return fmt.Errorf("exe를 바꿀 수 없어요. 폴더에 쓰기 권한이 있는지 확인하세요 (%v)", err)
	}
	if err := os.Rename(newPath, exe); err != nil {
		os.Rename(oldPath, exe) // 되돌리기
		os.Remove(newPath)
		return fmt.Errorf("exe를 바꾸지 못했어요 (%v)", err)
	}
	return nil
}

// 새 exe를 실행 (새 프로그램은 지금 프로그램이 꺼질 때까지 기다렸다가 시작)
func restartUpdated() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return exec.Command(exe, "--updated").Start()
}

var justUpdated bool

func anyInstance() bool {
	for p := 8000; p <= 8010; p++ {
		if runningInstance(p) {
			return true
		}
	}
	return false
}

// 업데이트 뒤 처음 실행: 옛 프로그램이 포트를 놓을 때까지 기다리고, .old 파일 정리
func afterUpdateStartup() {
	updated := false
	for _, a := range os.Args[1:] {
		if a == "--updated" {
			updated = true
		}
	}
	if updated {
		justUpdated = true
		for i := 0; i < 75 && anyInstance(); i++ {
			time.Sleep(200 * time.Millisecond)
		}
	}
	if exe, err := os.Executable(); err == nil {
		old := exe + ".old"
		go func() {
			for i := 0; i < 30; i++ {
				if err := os.Remove(old); err == nil || os.IsNotExist(err) {
					return
				}
				time.Sleep(time.Second)
			}
		}()
	}
}
