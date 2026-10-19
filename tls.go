package main

// 보안 주소(https): 아이패드 Safari는 http 주소에서 사진을 클립보드에 복사하지 못하게 막습니다.
// 이 PC 전용 인증서(로컬 인증기관)를 만들고, 기기에 한 번 설치해 신뢰하면
// https://IP:8443 에서 [복사]로 사진이 바로 복사됩니다.
//
// 인증기관은 사설 IP(10.x, 172.16~31.x, 192.168.x, 100.64~127.x)와 localhost에만 쓸 수 있도록 제한해서
// 혹시 키가 새어 나가도 다른 웹사이트를 사칭하는 데 쓸 수 없습니다.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	caCert    *x509.Certificate
	caKey     *ecdsa.PrivateKey
	caDER     []byte
	httpsPort int

	leafMu    sync.Mutex
	leafCache = map[string]*tls.Certificate{}
)

func certDir() string {
	d, err := os.UserConfigDir()
	if err != nil {
		d = shareDir
	}
	return filepath.Join(d, "파일공유")
}

func mustCIDR(s string) *net.IPNet {
	_, n, _ := net.ParseCIDR(s)
	return n
}

// 인증기관을 불러오거나 처음이면 새로 만듦 (기기에 설치한 인증서가 계속 유효하도록 보관)
func loadOrCreateCA() error {
	dir := certDir()
	certPath, keyPath := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	if c, k, err := readCA(certPath, keyPath); err == nil && time.Now().Before(c.NotAfter.AddDate(0, -1, 0)) {
		caCert, caKey, caDER = c, k, c.Raw
		return nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tpl := &x509.Certificate{
		SerialNumber:                serial,
		Subject:                     pkix.Name{CommonName: "파일공유 (" + host + ")", Organization: []string{"파일공유"}},
		NotBefore:                   time.Now().Add(-time.Hour),
		NotAfter:                    time.Now().AddDate(10, 0, 0),
		IsCA:                        true,
		BasicConstraintsValid:       true,
		MaxPathLenZero:              true,
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         []string{"localhost"},
		PermittedIPRanges: []*net.IPNet{
			mustCIDR("10.0.0.0/8"), mustCIDR("172.16.0.0/12"), mustCIDR("192.168.0.0/16"),
			mustCIDR("169.254.0.0/16"), mustCIDR("127.0.0.0/8"), mustCIDR("100.64.0.0/10"),
		},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	c, _ := x509.ParseCertificate(der)
	kb, _ := x509.MarshalECPrivateKey(key)
	if os.MkdirAll(dir, 0o700) == nil {
		os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
		os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	}
	caCert, caKey, caDER = c, key, der
	return nil
}

func readCA(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cb, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, err
	}
	kb, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, err
	}
	cp, _ := pem.Decode(cb)
	kp, _ := pem.Decode(kb)
	if cp == nil || kp == nil {
		return nil, nil, errors.New("bad pem")
	}
	c, err := x509.ParseCertificate(cp.Bytes)
	if err != nil {
		return nil, nil, err
	}
	k, err := x509.ParseECPrivateKey(kp.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return c, k, nil
}

// 접속한 IP에 맞는 서버 인증서 (IP가 바뀌면 새 IP용을 바로 만들어 씀)
func leafFor(ip net.IP) (*tls.Certificate, error) {
	key := ip.String()
	leafMu.Lock()
	defer leafMu.Unlock()
	if c, ok := leafCache[key]; ok && time.Now().Before(c.Leaf.NotAfter.AddDate(0, 0, -7)) {
		return c, nil
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: key},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(0, 0, 390), // 애플 기준 398일 이하
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{ip, net.IPv4(127, 0, 0, 1)},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, caCert, &k.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	leaf, _ := x509.ParseCertificate(der)
	c := &tls.Certificate{Certificate: [][]byte{der, caDER}, PrivateKey: k, Leaf: leaf}
	leafCache[key] = c
	return c, nil
}

// https 서버 시작 (8443부터 빈 포트). 실패해도 http는 그대로 동작
func startHTTPS(h http.Handler) {
	if err := loadOrCreateCA(); err != nil {
		return
	}
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			ip := net.ParseIP(localIP())
			if a, ok := hello.Conn.LocalAddr().(*net.TCPAddr); ok && a.IP != nil {
				ip = a.IP
			}
			if v4 := ip.To4(); v4 != nil {
				ip = v4
			}
			return leafFor(ip)
		},
	}
	for p := 8443; p <= 8453; p++ {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
		if err != nil {
			continue
		}
		httpsPort = p
		srv := &http.Server{Handler: h, TLSConfig: cfg, ErrorLog: log.New(io.Discard, "", 0)}
		go srv.Serve(tls.NewListener(ln, cfg))
		return
	}
}

func httpsURL() string {
	if httpsPort == 0 {
		return ""
	}
	return fmt.Sprintf("https://%s:%d", localIP(), httpsPort)
}

// 기기에 설치할 인증서 (아이폰·아이패드는 받으면 '프로파일 설치' 안내가 뜸)
func handleCACert(w http.ResponseWriter, r *http.Request) {
	if caDER == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/x-x509-ca-cert")
	w.Header().Set("Content-Disposition", `attachment; filename="fileshare-ca.crt"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(caDER)
}
