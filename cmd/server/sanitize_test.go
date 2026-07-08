package main

import (
	"testing"
	"unicode/utf8"
)

func TestSanitizeUTF8(t *testing.T) {
	// GBK bytes for "版本" (the culprit from a Chinese Windows `cmd /c ver`:
	// "Microsoft Windows [版本 10.0.22631.4317]"). 0xB0 is the byte Postgres
	// rejected: `invalid byte sequence for encoding "UTF8": 0xb0`.
	gbk := "Microsoft Windows [\xb0\xe6\xb1\xbe 10.0.22631.4317]"

	tests := []struct {
		name string
		in   string
		want string // "" means: don't assert exact, just require valid + ASCII kept
	}{
		{"ascii passthrough", "Microsoft Windows [Version 10.0.22631]", "Microsoft Windows [Version 10.0.22631]"},
		{"valid utf8 kept", "Ubuntu 22.04 LTS 中文", "Ubuntu 22.04 LTS 中文"},
		{"empty", "", ""},
		{"gbk stripped to valid", gbk, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeUTF8(tc.in)
			if !utf8.ValidString(got) {
				t.Fatalf("result is not valid UTF-8: %q", got)
			}
			if tc.want != "" && got != tc.want {
				t.Fatalf("sanitizeUTF8(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	// The bad-byte case must survive (row would now persist) while keeping the
	// ASCII build number the operator actually cares about.
	got := sanitizeUTF8(gbk)
	if !utf8.ValidString(got) {
		t.Fatalf("GBK input still invalid UTF-8: %q", got)
	}
	if want := "10.0.22631.4317"; !containsSub(got, want) {
		t.Errorf("expected build number %q preserved, got %q", want, got)
	}
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
