package access

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileProblemSelf(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("от root права не ограничены")
	}
	c, err := Self()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	os.Mkdir(locked, 0o755)
	f := filepath.Join(locked, "a.log")
	os.WriteFile(f, []byte("x"), 0o644)

	if p := c.FileProblem(f); p != nil {
		t.Fatalf("читаемый файл: %+v", p)
	}

	os.Chmod(f, 0o000)
	p := c.FileProblem(f)
	if p == nil || !strings.Contains(p.Fix, "setfacl -m u:"+c.Name()+":r") || !strings.Contains(p.Fix, "-d -m") {
		t.Errorf("нечитаемый файл: %+v", p)
	}
	os.Chmod(f, 0o644)

	os.Chmod(locked, 0o600)
	defer os.Chmod(locked, 0o755)
	p = c.FileProblem(f)
	if p == nil || !strings.Contains(p.Fix, ":x "+locked) {
		t.Errorf("закрытый каталог: %+v", p)
	}

	if p := c.FileProblem(filepath.Join(dir, "nope.log")); p == nil {
		t.Error("несуществующий файл")
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"/srv/minecraft": "/srv/minecraft",
		"/a b/c":         "'/a b/c'",
		"/it's":          `'/it'\''s'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("%q → %s, want %s", in, got, want)
		}
	}
}
