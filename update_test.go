package main

import "testing"

func TestNewerVersion(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{"1.10", "1.9", true}, {"1.9", "1.10", false}, {"1.9.1", "1.9", true}, {"1.9", "1.9", false}, {"2", "1.9.9", true}} {
		if got := newerVersion(c.a, c.b); got != c.want {
			t.Errorf("newerVersion(%q,%q)=%v", c.a, c.b, got)
		}
	}
	if r := resolveRef("https://x.com/a/version.json", "파일공유.exe"); r != "https://x.com/a/%ED%8C%8C%EC%9D%BC%EA%B3%B5%EC%9C%A0.exe" && r != "https://x.com/a/파일공유.exe" {
		t.Errorf("resolveRef=%s", r)
	}
}
