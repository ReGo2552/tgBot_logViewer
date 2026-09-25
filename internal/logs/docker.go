package logs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ReGo2552/tgBot_logViewer/internal/config"
)

type Container struct {
	Name    string `json:"name"`
	Image   string `json:"image"`
	State   string `json:"state"`
	Project string `json:"project,omitempty"`
}

// Docker читает контейнеры либо сам (access: direct), либо через
// `sudo -n logviewer docker-helper …` (access: sudo) — тогда проверку имени
// контейнера по конфигу повторяет root-процесс.
type Docker struct {
	Binary string
	Sudo   bool
	Helper string // путь к logviewer для sudo; совпадает с правилом в sudoers
	Cmd    CommandFunc
}

func NewDocker(cfg *config.Config) *Docker {
	return &Docker{
		Binary: cfg.Docker.Binary,
		Sudo:   cfg.Docker.Access == config.DockerSudo,
		Helper: config.BinaryPath,
		Cmd:    DefaultCommand,
	}
}

func (d *Docker) List(ctx context.Context) ([]Container, error) {
	if d.Sudo {
		var cs []Container
		return cs, d.helper(ctx, &cs, "ps")
	}
	return ListDirect(ctx, d.Cmd, d.Binary)
}

func (d *Docker) Logs(ctx context.Context, name string, n int) ([]Line, error) {
	if !config.ValidContainerName(name) {
		return nil, fmt.Errorf("недопустимое имя контейнера %q", name)
	}
	if d.Sudo {
		var ls []Line
		return ls, d.helper(ctx, &ls, "logs", name, strconv.Itoa(n))
	}
	return LogsDirect(ctx, d.Cmd, d.Binary, name, n)
}

func (d *Docker) helper(ctx context.Context, v any, args ...string) error {
	o, err := runCmd(ctx, d.Cmd, "sudo", append([]string{"-n", d.Helper, "docker-helper"}, args...)...)
	if err != nil {
		if strings.Contains(err.Error(), "password is required") {
			return errors.New("sudo не пускает к docker: нет правила /etc/sudoers.d/logviewer (переустановите: sudo logviewer install)")
		}
		return err
	}
	if err := json.Unmarshal(o.stdout, v); err != nil {
		return fmt.Errorf("docker-helper: непонятный ответ: %w", err)
	}
	return nil
}

// Matches — относится ли контейнер к источнику.
func Matches(s config.Source, c Container) bool {
	switch {
	case s.Type != config.TypeDocker:
		return false
	case s.Container != "":
		return s.Container == c.Name
	case strings.HasPrefix(s.Match, "stack:"):
		return c.Project != "" && c.Project == strings.TrimPrefix(s.Match, "stack:")
	case s.Match != "":
		ok, _ := path.Match(s.Match, c.Name)
		return ok
	}
	return false
}

// AllowedBy — контейнер разрешён хотя бы одним источником конфига.
func AllowedBy(cfg *config.Config, c Container) bool {
	for _, s := range cfg.Sources {
		if Matches(s, c) {
			return true
		}
	}
	return false
}

func ListDirect(ctx context.Context, mk CommandFunc, bin string) ([]Container, error) {
	o, err := runCmd(ctx, mk, bin, "ps", "--all", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	var cs []Container
	for _, raw := range bytes.Split(o.stdout, []byte{'\n'}) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var p struct{ Names, Image, State, Labels string }
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("docker ps: %w", err)
		}
		c := Container{Name: p.Names, Image: p.Image, State: p.State}
		for _, kv := range strings.Split(p.Labels, ",") {
			if v, ok := strings.CutPrefix(kv, "com.docker.compose.project="); ok {
				c.Project = v
			}
		}
		cs = append(cs, c)
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].Name < cs[j].Name })
	return cs, nil
}

// LogsDirect: stdout и stderr контейнера docker отдаёт разными потоками, поэтому
// берём их с метками времени и сливаем по времени — иначе ошибки уезжают от контекста.
func LogsDirect(ctx context.Context, mk CommandFunc, bin, name string, n int) ([]Line, error) {
	out := &tailBuffer{limit: MaxOutputBytes / 2}
	errb := &tailBuffer{limit: MaxOutputBytes / 2}
	cmd := mk(ctx, bin, "logs", "--timestamps", "--tail", strconv.Itoa(n), name)
	cmd.Stdout = out
	cmd.Stderr = errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("docker logs: не уложился во время (%w)", ctx.Err())
		}
		if msg := firstLine(errb.Bytes()); msg != "" {
			return nil, fmt.Errorf("docker logs: %s", msg)
		}
		return nil, fmt.Errorf("docker logs: %w", err)
	}
	lines := append(parseDocker(out.Bytes()), parseDocker(errb.Bytes())...)
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].Time.Before(lines[j].Time) })
	return lastN(lines, n), nil
}

func parseDocker(data []byte) []Line {
	var lines []Line
	for _, raw := range strings.Split(string(data), "\n") {
		if raw == "" {
			continue
		}
		l := Line{Text: raw}
		if ts, text, ok := strings.Cut(raw, " "); ok {
			if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
				l = Line{Time: t, Text: text}
			}
		}
		lines = append(lines, l)
	}
	return lines
}
