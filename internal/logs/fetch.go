package logs

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/ReGo2552/tgBot_logViewer/internal/config"
	"github.com/ReGo2552/tgBot_logViewer/internal/redact"
)

// Target — конкретная цель источника: контейнер стека или файл из glob.
type Target struct {
	Name  string // имя контейнера или путь
	Label string // подпись кнопки
}

type Request struct {
	Source config.Source
	Target string // для IsMulti-источников; иначе пусто
	Lines  int
	Filter string
}

type Result struct {
	Lines   []Line
	Target  string // что именно читали: юнит, контейнер, путь
	Scanned int    // при фильтре: сколько строк просмотрено
}

type Fetcher struct {
	Cfg      *config.Config
	Docker   *Docker
	Redactor *redact.Redactor
	Cmd      CommandFunc
}

// Targets раскрывает источник в список целей. Для обычного источника — одна цель.
func (f *Fetcher) Targets(ctx context.Context, s config.Source) ([]Target, error) {
	switch s.Type {
	case config.TypeSystemd:
		return []Target{{Name: s.Unit, Label: s.Unit}}, nil
	case config.TypeDocker:
		if s.Container != "" {
			return []Target{{Name: s.Container, Label: s.Container}}, nil
		}
		cs, err := f.Docker.List(ctx)
		if err != nil {
			return nil, err
		}
		var ts []Target
		for _, c := range cs {
			if Matches(s, c) {
				label := c.Name
				if c.State != "running" {
					label += " (" + c.State + ")"
				}
				ts = append(ts, Target{Name: c.Name, Label: label})
			}
		}
		return ts, nil
	case config.TypeFile:
		files, err := ResolveFiles(s)
		if err != nil {
			return nil, err
		}
		ts := make([]Target, len(files))
		for i, p := range files {
			ts[i] = Target{Name: p, Label: shortPath(p)}
		}
		return ts, nil
	}
	return nil, fmt.Errorf("неизвестный тип %q", s.Type)
}

func (f *Fetcher) Fetch(ctx context.Context, req Request) (*Result, error) {
	s := req.Source
	target, err := f.resolve(ctx, s, req.Target)
	if err != nil {
		return nil, err
	}

	n := f.Cfg.ClampLines(req.Lines)
	window := n
	if req.Filter != "" {
		window = f.Cfg.Lines.FilterWindow
	}

	var lines []Line
	switch s.Type {
	case config.TypeSystemd:
		lines, err = Journal(ctx, f.Cmd, target, window)
	case config.TypeDocker:
		lines, err = f.Docker.Logs(ctx, target, window)
	case config.TypeFile:
		lines, err = TailFile(target, window)
	}
	if err != nil {
		return nil, err
	}

	res := &Result{Target: target, Scanned: len(lines)}
	needle := strings.ToLower(req.Filter)
	out := lines[:0]
	for _, l := range lines {
		l.Text = redact.Clean(l.Text)
		if needle != "" && !strings.Contains(strings.ToLower(l.Text), needle) {
			continue
		}
		l.Text = f.Redactor.Mask(l.Text)
		out = append(out, l)
	}
	res.Lines = lastN(out, n)
	return res, nil
}

// resolve проверяет, что цель действительно принадлежит источнику: имя контейнера
// и путь приходят из кнопок, а кнопка могла устареть.
func (f *Fetcher) resolve(ctx context.Context, s config.Source, target string) (string, error) {
	if !s.IsMulti() {
		switch s.Type {
		case config.TypeSystemd:
			return s.Unit, nil
		case config.TypeDocker:
			return s.Container, nil
		default:
			return s.Path, nil
		}
	}
	ts, err := f.Targets(ctx, s)
	if err != nil {
		return "", err
	}
	if target == "" {
		if len(ts) == 1 {
			return ts[0].Name, nil
		}
		return "", fmt.Errorf("у источника %s несколько целей, выберите одну", s.ID)
	}
	if !slices.ContainsFunc(ts, func(t Target) bool { return t.Name == target }) {
		return "", fmt.Errorf("%s больше не относится к источнику %s", target, s.ID)
	}
	return target, nil
}

func shortPath(p string) string {
	parts := strings.Split(p, "/")
	if len(parts) > 3 {
		return "…/" + strings.Join(parts[len(parts)-2:], "/")
	}
	return p
}
