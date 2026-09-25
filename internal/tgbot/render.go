package tgbot

import (
	"fmt"
	"html"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ReGo2552/tgBot_logViewer/internal/config"
	"github.com/ReGo2552/tgBot_logViewer/internal/logs"
)

// Лимит сообщения Telegram — 4096 символов; остаток — на заголовок.
const maxInlineRunes = 3500

var typeTitles = map[string]string{
	config.TypeSystemd: "⚙️ Службы",
	config.TypeDocker:  "🐳 Контейнеры",
	config.TypeFile:    "📄 Файлы",
}

var typeOrder = []string{config.TypeSystemd, config.TypeDocker, config.TypeFile}

func esc(s string) string { return html.EscapeString(s) }

// body — строки лога одним текстом, время в поясе из конфига.
func body(lines []logs.Line, loc *time.Location) string {
	var b strings.Builder
	for i, l := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		if !l.Time.IsZero() {
			b.WriteString(l.Time.In(loc).Format("01-02 15:04:05"))
			b.WriteByte(' ')
		}
		b.WriteString(l.Text)
	}
	return b.String()
}

func header(s config.Source, q query, res *logs.Result, loc *time.Location) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<b>%s</b>", esc(s.Label()))
	switch s.Type {
	case config.TypeSystemd:
		fmt.Fprintf(&b, " · journald <code>%s</code>", esc(res.Target))
	case config.TypeDocker:
		fmt.Fprintf(&b, " · docker <code>%s</code>", esc(res.Target))
	case config.TypeFile:
		fmt.Fprintf(&b, " · <code>%s</code>", esc(res.Target))
	}
	b.WriteByte('\n')
	if q.Filter != "" {
		fmt.Fprintf(&b, "🔎 «%s»: %d совп. в последних %d строках", esc(q.Filter), len(res.Lines), res.Scanned)
	} else {
		fmt.Fprintf(&b, "%d строк", len(res.Lines))
	}
	if first, last := timeRange(res.Lines); !first.IsZero() {
		fmt.Fprintf(&b, " · %s → %s", first.In(loc).Format("01-02 15:04"), last.In(loc).Format("01-02 15:04"))
	}
	return b.String()
}

func timeRange(lines []logs.Line) (time.Time, time.Time) {
	var first, last time.Time
	for _, l := range lines {
		if l.Time.IsZero() {
			continue
		}
		if first.IsZero() {
			first = l.Time
		}
		last = l.Time
	}
	return first, last
}

func fitsInline(text string) bool { return utf8.RuneCountInString(text) <= maxInlineRunes }

func fileName(s config.Source, res *logs.Result, now time.Time) string {
	name := s.ID
	if s.IsMulti() {
		t := res.Target
		if i := strings.LastIndexByte(t, '/'); i >= 0 {
			t = t[i+1:]
		}
		name += "_" + t
	}
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == ' ' || r < 32 {
			return '_'
		}
		return r
	}, name)
	return strings.TrimSuffix(name, ".log") + "_" + now.Format("2006-01-02_150405") + ".txt"
}

const helpText = `<b>Логи сервера</b>

/logs — выбрать источник кнопками
/logs <code>id</code> — последние строки источника
/logs <code>id 300</code> — 300 строк
/logs <code>id 500 error</code> — строки с «error» среди последних (фильтр без учёта регистра)
/discover — что есть на сервере, но не добавлено в конфиг
/doctor — проверить доступ к источникам

Под логом: число строк, 🔄 обновить, 📎 прислать файлом.
Секреты (токены, пароли, user:pass@) в выводе маскируются.`
