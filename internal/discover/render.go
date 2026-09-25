package discover

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ReGo2552/tgBot_logViewer/internal/config"
	"github.com/ReGo2552/tgBot_logViewer/internal/logs"
)

type Meta struct {
	Host      string
	Admins    []int64
	Timezone  string
	User      string // от чьего имени проверялись права
	Generated time.Time
}

// RenderConfig — полный черновик config.yaml с пояснениями.
func RenderConfig(r *Result, m Meta) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }

	w("# Конфиг logviewer. Черновик: `logviewer discover`, %s, хост %s.\n",
		m.Generated.Format("2006-01-02 15:04"), m.Host)
	w("#\n")
	w("# Бот показывает ТОЛЬКО источники из списка sources. Закомментированные строки —\n")
	w("# найденные кандидаты: раскомментируйте нужные, лишние удалите.\n")
	if m.User != "" {
		w("# Права на файлы проверены для пользователя %s: строки с ⚠ он пока не прочитает.\n", m.User)
	}
	w("# Проверить результат: sudo logviewer doctor\n\n")

	w("# Telegram ID тех, кому бот отвечает. Узнать свой: @userinfobot.\n")
	if len(m.Admins) == 0 {
		w("admins: []  # ← впишите свой ID, иначе бот не запустится\n\n")
	} else {
		ids := make([]string, len(m.Admins))
		for i, id := range m.Admins {
			ids[i] = strconv.FormatInt(id, 10)
		}
		w("admins: [%s]\n\n", strings.Join(ids, ", "))
	}

	w("# Часовой пояс для времени в логах (journald и docker хранят UTC).\n")
	w("# Пусто — пояс сервера. Пример: Europe/Moscow, Asia/Novosibirsk.\n")
	w("timezone: %s\n\n", yamlStr(m.Timezone))

	w("lines:\n")
	w("  default: 100         # сколько строк показывать по умолчанию\n")
	w("  max: 1000            # больше за один запрос не отдаём\n")
	w("  filter_window: 5000  # среди скольких последних строк искать при фильтре\n\n")

	w("docker:\n")
	w("  binary: docker\n")
	w("  # sudo   — бот ходит в docker через sudo-правило на одну команду (рекомендуется);\n")
	w("  # direct — пользователь бота сам в группе docker, а это равносильно root.\n")
	w("  access: sudo\n\n")

	w("# Маскирование секретов (токены, пароли, user:pass@ в URL). Свои шаблоны — в extra.\n")
	w("redact:\n")
	w("  disable: false\n")
	w("  extra: []\n\n")

	w("sources:\n")
	writeSection(&b, "systemd: службы", r.Systemd)
	writeSection(&b, "docker: контейнеры (стек = все контейнеры compose-проекта)", r.Docker)
	writeSection(&b, "файлы", r.Files)
	if len(r.Systemd)+len(r.Docker)+len(r.Files) == 0 {
		w("  []\n")
	}
	if len(r.Warnings) > 0 {
		w("\n# Предупреждения при сканировании:\n")
		for _, s := range r.Warnings {
			w("#   %s\n", oneLine(s))
		}
	}
	return b.String()
}

// RenderSnippet — только строки sources, для вставки в существующий конфиг.
func RenderSnippet(cs []Candidate) string {
	var b strings.Builder
	for _, c := range cs {
		writeCandidate(&b, c)
	}
	return b.String()
}

func writeSection(b *strings.Builder, title string, cs []Candidate) {
	if len(cs) == 0 {
		return
	}
	fmt.Fprintf(b, "\n  # --- %s\n", title)
	for _, c := range cs {
		writeCandidate(b, c)
	}
}

func writeCandidate(b *strings.Builder, c Candidate) {
	if c.Problem != nil {
		fmt.Fprintf(b, "  # ⚠ %s\n", oneLine(c.Problem.What))
		if c.Problem.Fix != "" {
			fmt.Fprintf(b, "  #   исправить: %s\n", c.Problem.Fix)
		}
	}
	prefix := "  "
	if !c.Enabled {
		prefix = "  # "
	}
	fmt.Fprintf(b, "%s- %s", prefix, flow(c.Source))
	if c.Note != "" {
		fmt.Fprintf(b, "  # %s", oneLine(c.Note))
	}
	b.WriteByte('\n')
}

func flow(s config.Source) string {
	parts := []string{"id: " + yamlStr(s.ID), "type: " + s.Type}
	switch {
	case s.Unit != "":
		parts = append(parts, "unit: "+yamlStr(s.Unit))
	case s.Container != "":
		parts = append(parts, "container: "+yamlStr(s.Container))
	case s.Match != "":
		parts = append(parts, "match: "+yamlStr(s.Match))
	case s.Path != "":
		parts = append(parts, "path: "+yamlStr(s.Path))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

var rePlain = regexp.MustCompile(`^[A-Za-z0-9_./@-][A-Za-z0-9_./@+-]*$`)

// yamlStr — скаляр для flow-стиля: без кавычек, если можно, иначе в двойных.
func yamlStr(s string) string {
	if s != "" && rePlain.MatchString(s) && !isYAMLKeyword(s) {
		return s
	}
	return strconv.Quote(s)
}

func isYAMLKeyword(s string) bool {
	switch strings.ToLower(s) {
	case "true", "false", "yes", "no", "on", "off", "null", "~", "y", "n":
		return true
	}
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len([]rune(s)) > 160 {
		s = string([]rune(s)[:160]) + "…"
	}
	return s
}

// Missing — включённые кандидаты, которых ещё нет в конфиге. Используется в /discover.
func Missing(cfg *config.Config, r *Result) *Result {
	out := &Result{Containers: r.Containers, Warnings: r.Warnings}
	for _, c := range r.Systemd {
		if c.Enabled && !hasUnit(cfg, c.Source.Unit) {
			out.Systemd = append(out.Systemd, c)
		}
	}
	for _, c := range r.Docker {
		if c.Enabled && !dockerCovered(cfg, c.Source, r.Containers) {
			out.Docker = append(out.Docker, c)
		}
	}
	for _, c := range r.Files {
		if c.Enabled && !hasFile(cfg, c.Source.Path) {
			out.Files = append(out.Files, c)
		}
	}
	taken := map[string]bool{}
	for _, s := range cfg.Sources {
		taken[s.ID] = true
	}
	out.AssignIDs(taken)
	return out
}

func hasUnit(cfg *config.Config, unit string) bool {
	for _, s := range cfg.Sources {
		if s.Type == config.TypeSystemd && s.Unit == unit {
			return true
		}
	}
	return false
}

func hasFile(cfg *config.Config, p string) bool {
	for _, s := range cfg.Sources {
		if s.Type != config.TypeFile {
			continue
		}
		if ok, _ := filepath.Match(s.Path, p); ok || s.Path == p {
			return true
		}
	}
	return false
}

// dockerCovered — все контейнеры кандидата уже попадают под какой-то источник конфига.
func dockerCovered(cfg *config.Config, cand config.Source, cs []logs.Container) bool {
	found := false
	for _, c := range cs {
		if !logs.Matches(cand, c) {
			continue
		}
		found = true
		if !logs.AllowedBy(cfg, c) {
			return false
		}
	}
	return found
}
