package tgbot

import (
	"strings"
	"testing"
	"time"

	"github.com/ReGo2552/tgBot_logViewer/internal/config"
	"github.com/ReGo2552/tgBot_logViewer/internal/logs"
)

func TestStoreEviction(t *testing.T) {
	s := newStore(3)
	first := s.put(query{SourceID: "a"})
	for i := 0; i < 3; i++ {
		s.put(query{SourceID: "b"})
	}
	if _, ok := s.get(first); ok {
		t.Error("старый ключ должен вытесняться")
	}
	k := s.put(query{SourceID: "c", Target: strings.Repeat("x", 200)})
	if q, ok := s.get(k); !ok || q.SourceID != "c" {
		t.Error("новый ключ")
	}
	if len("k:"+k) > 64 {
		t.Error("callback_data длиннее 64 байт")
	}
}

func TestRender(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Novosibirsk")
	ts := time.Date(2026, 9, 25, 16, 38, 25, 0, time.UTC)
	lines := []logs.Line{{Time: ts, Text: "<b>not bold</b> & co"}, {Text: "no time"}}
	if got := body(lines, loc); got != "09-25 23:38:25 <b>not bold</b> & co\nno time" {
		t.Errorf("body: %q", got)
	}
	s := config.Source{ID: "caddy", Type: config.TypeSystemd, Unit: "caddy.service"}
	h := header(s, query{Filter: "<err>"}, &logs.Result{Lines: lines, Target: "caddy.service", Scanned: 5000}, loc)
	if !strings.Contains(h, "«&lt;err&gt;»: 2 совп. в последних 5000") || !strings.Contains(h, "09-25 23:38") {
		t.Errorf("header: %s", h)
	}
	if esc("<pre>") != "&lt;pre&gt;" {
		t.Error("esc")
	}
	if !fitsInline(strings.Repeat("я", maxInlineRunes)) || fitsInline(strings.Repeat("я", maxInlineRunes+1)) {
		t.Error("fitsInline считает символы, а не байты")
	}
}

func TestFileName(t *testing.T) {
	now := time.Date(2026, 9, 25, 16, 38, 25, 0, time.UTC)
	s := config.Source{ID: "mc", Type: config.TypeFile, Path: "/srv/*/logs/*.log"}
	got := fileName(s, &logs.Result{Target: "/srv/minecraft/logs/latest.log"}, now)
	if got != "mc_latest_2026-09-25_163825.txt" {
		t.Errorf("fileName: %s", got)
	}
}
