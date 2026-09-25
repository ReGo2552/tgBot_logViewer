package logs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ReGo2552/tgBot_logViewer/internal/config"
	"github.com/ReGo2552/tgBot_logViewer/internal/redact"
)

func TestParseJournal(t *testing.T) {
	data := `{"__REALTIME_TIMESTAMP":"1758817105000000","MESSAGE":"first"}
{"__REALTIME_TIMESTAMP":"1758817106000000","MESSAGE":[104,105,255]}
{"__REALTIME_TIMESTAMP":"1758817107000000","MESSAGE":"multi\nline"}
{"__REALTIME_TIMESTAMP":"1758817108000000","MESSAGE":null}
{"__REALTIME_TIMESTAMP":"17588171`
	lines := parseJournal([]byte(data), 10)
	if len(lines) != 5 {
		t.Fatalf("строк %d: %+v", len(lines), lines)
	}
	if lines[1].Text != "hi\xff" || lines[2].Text != "multi" || lines[3].Text != "line" {
		t.Errorf("разбор MESSAGE: %+v", lines)
	}
	if !lines[0].Time.Equal(time.UnixMicro(1758817105000000)) {
		t.Errorf("время: %v", lines[0].Time)
	}
	if got := parseJournal([]byte(data), 2); len(got) != 2 || got[0].Text != "line" {
		t.Errorf("последние 2: %+v", got)
	}
}

func TestParseDockerMerge(t *testing.T) {
	stdout := "2026-09-25T10:00:01.000000001Z out one\n2026-09-25T10:00:03Z out two\nno timestamp\n"
	stderr := "2026-09-25T10:00:02Z err one\n"
	// эмулируем docker: stdout и stderr разными потоками
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "out"), []byte(stdout), 0o644)
	os.WriteFile(filepath.Join(dir, "err"), []byte(stderr), 0o644)
	mk := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", `cat "$0/out"; cat "$0/err" >&2`, dir)
	}
	lines, err := LogsDirect(context.Background(), mk, "docker", "x", 10)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range lines {
		got = append(got, l.Text)
	}
	// строка без времени — нулевое время, уходит в начало
	want := "no timestamp|out one|err one|out two"
	if strings.Join(got, "|") != want {
		t.Errorf("порядок: %v", got)
	}
}

func TestTailFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.log")
	var b strings.Builder
	for i := 1; i <= 50000; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	os.WriteFile(p, []byte(b.String()), 0o644)

	lines, err := TailFile(p, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 || lines[0].Text != "line 49998" || lines[2].Text != "line 50000" {
		t.Errorf("хвост: %+v", lines)
	}
	lines, _ = TailFile(p, 20000) // больше одного куска 64 КБ
	if len(lines) != 20000 || lines[0].Text != "line 30001" {
		t.Errorf("20000 строк: %d, первая %q", len(lines), lines[0].Text)
	}

	// файл без перевода строки в конце и короче n
	os.WriteFile(p, []byte("x\ny"), 0o644)
	lines, _ = TailFile(p, 10)
	if len(lines) != 2 || lines[1].Text != "y" {
		t.Errorf("короткий: %+v", lines)
	}
	os.WriteFile(p, nil, 0o644)
	if lines, _ = TailFile(p, 10); len(lines) != 0 {
		t.Errorf("пустой: %+v", lines)
	}
}

func TestTailBuffer(t *testing.T) {
	tb := &tailBuffer{limit: 10}
	for i := 0; i < 10; i++ {
		fmt.Fprintf(tb, "line%d\n", i)
	}
	if got := string(tb.Bytes()); got != "line9\n" {
		t.Errorf("хвост буфера: %q", got)
	}
}

func TestMatches(t *testing.T) {
	c := Container{Name: "immich_server", Project: "immich"}
	cases := map[string]bool{
		"stack:immich": true, "stack:other": false, "immich_*": true, "*": true, "nextcloud*": false,
	}
	for m, want := range cases {
		if got := Matches(config.Source{Type: config.TypeDocker, Match: m}, c); got != want {
			t.Errorf("match %q = %v", m, got)
		}
	}
	if !Matches(config.Source{Type: config.TypeDocker, Container: "immich_server"}, c) {
		t.Error("container")
	}
	if Matches(config.Source{Type: config.TypeDocker, Match: "stack:"}, Container{Name: "x"}) {
		t.Error("пустой проект не должен совпадать")
	}
}

func TestFetchFileFilterAndRedact(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bot.log")
	os.WriteFile(p, []byte("start\nERROR one password=abc\nok\nerror two\n\x1b[31mError three\x1b[0m\n"), 0o644)
	cfg, err := config.Parse([]byte(fmt.Sprintf("sources: [{id: bot, type: file, path: %s}]", p)))
	if err != nil {
		t.Fatal(err)
	}
	red, _ := redact.New(false, nil)
	f := &Fetcher{Cfg: cfg, Redactor: red, Cmd: DefaultCommand}
	s, _ := cfg.Source("bot")
	res, err := f.Fetch(context.Background(), Request{Source: s, Lines: 2, Filter: "error"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Scanned != 5 || len(res.Lines) != 2 || res.Lines[0].Text != "error two" || res.Lines[1].Text != "Error three" {
		t.Errorf("фильтр: %+v", res)
	}
	res, _ = f.Fetch(context.Background(), Request{Source: s, Lines: 4})
	if res.Lines[0].Text != "ERROR one password=***" {
		t.Errorf("маскирование: %+v", res.Lines)
	}
}

func TestResolveGlobTarget(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.log", "b.log", "c.txt"} {
		os.WriteFile(filepath.Join(dir, n), []byte("x\n"), 0o644)
	}
	cfg, _ := config.Parse([]byte(fmt.Sprintf("sources: [{id: g, type: file, path: %q}]", dir+"/*.log")))
	red, _ := redact.New(false, nil)
	f := &Fetcher{Cfg: cfg, Redactor: red}
	s, _ := cfg.Source("g")
	ts, _ := f.Targets(context.Background(), s)
	if len(ts) != 2 {
		t.Fatalf("цели: %+v", ts)
	}
	if _, err := f.Fetch(context.Background(), Request{Source: s, Target: filepath.Join(dir, "c.txt")}); err == nil {
		t.Error("файл вне glob не должен читаться")
	}
	if _, err := f.Fetch(context.Background(), Request{Source: s}); err == nil {
		t.Error("без цели при нескольких файлах должна быть ошибка")
	}
	if _, err := f.Fetch(context.Background(), Request{Source: s, Target: ts[1].Name}); err != nil {
		t.Error(err)
	}
}
