package redact

import (
	"strings"
	"testing"
)

func TestMask(t *testing.T) {
	r, err := New(false, []string{`\b\d{3}-\d{2}-\d{4}\b`})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ in, want string }{
		{"PROXY_URL=socks5h://myuser:s3cret@1.2.3.4:9966", "PROXY_URL=socks5h://***:***@1.2.3.4:9966"},
		{"token 1234567890:AAaaBBbbCCccDDddEEeeFFffGGgghhhhiii ok", "token <tg-token> ok"},
		{"BOT_TOKEN=abc DB_PASSWORD=qwerty api_key: zzz", "BOT_TOKEN=*** DB_PASSWORD=*** api_key: ***"},
		{`{"password": "hunter2", "user": "alice"}`, `{"password": ***, "user": "alice"}`},
		{"Authorization: Basic dXNlcjpwYXNz", "Authorization: Basic ***"},
		{"Authorization: Bearer abcdefghijkl", "Authorization: Bearer ***"},
		{"jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N", "jwt <jwt>"},
		{"key AKIAABCDEFGHIJKLMNOP", "key <aws-key>"},
		{"ssn 123-45-6789", "ssn ***"},
		// обычные строки не трогаем
		{"[Server thread/INFO]: Alice joined the game", "[Server thread/INFO]: Alice joined the game"},
		{"GET /index.php 200 127.0.0.1:8090", "GET /index.php 200 127.0.0.1:8090"},
		{"[AuthMe] Alice logged in", "[AuthMe] Alice logged in"},
	}
	for _, c := range cases {
		if got := r.Mask(c.in); got != c.want {
			t.Errorf("Mask(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func TestDisable(t *testing.T) {
	r, _ := New(true, nil)
	if s := "password=x"; r.Mask(s) != s {
		t.Error("disable должен выключать маскирование")
	}
}

func TestClean(t *testing.T) {
	cases := []struct{ in, want string }{
		{"\x1b[32mINFO\x1b[0m started", "INFO started"},
		{"a\tb\r\n", "a b"},
		{"bell\x07 and \x00nul", "bell and nul"},
		{"bad \xff utf8", "bad � utf8"},
		{"\x1b]0;title\x07text", "text"},
	}
	for _, c := range cases {
		if got := Clean(c.in); got != c.want {
			t.Errorf("Clean(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	long := strings.Repeat("я", MaxLineRunes+10)
	if got := Clean(long); len([]rune(got)) != MaxLineRunes+1 || !strings.HasSuffix(got, "…") {
		t.Errorf("длинная строка не обрезана: %d", len([]rune(got)))
	}
}
