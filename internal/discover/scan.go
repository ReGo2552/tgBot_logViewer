// Package discover ищет кандидатов в источники логов и пишет из них черновик
// конфига. Он ничего не решает за человека: неочевидное выводится закомментированным.
package discover

import (
	"bufio"
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/ReGo2552/tgBot_logViewer/internal/access"
	"github.com/ReGo2552/tgBot_logViewer/internal/config"
	"github.com/ReGo2552/tgBot_logViewer/internal/logs"
)

type Candidate struct {
	Source  config.Source
	Enabled bool   // попадёт в конфиг раскомментированным
	Note    string // откуда взялся
	Problem *access.Problem
}

type Options struct {
	Cmd     logs.CommandFunc // для systemctl
	Docker  *logs.Docker     // nil — docker не сканируем
	Checker *access.Checker  // nil — права не проверяем
	Self    string           // имя своего юнита, чтобы не предлагать самого себя
}

type Result struct {
	Systemd    []Candidate
	Docker     []Candidate
	Files      []Candidate
	Containers []logs.Container
	Warnings   []string
}

func (r *Result) All() []Candidate {
	return append(append(append([]Candidate{}, r.Systemd...), r.Docker...), r.Files...)
}

func Scan(ctx context.Context, o Options) *Result {
	if o.Cmd == nil {
		o.Cmd = logs.DefaultCommand
	}
	r := &Result{}

	units, err := listUnits(ctx, o.Cmd)
	if err != nil {
		r.Warnings = append(r.Warnings, "systemd: "+err.Error())
	}
	var workdirs []workdir
	for _, u := range units {
		if u.ID == o.Self {
			continue
		}
		c, ok := classifyUnit(u)
		if !ok {
			continue
		}
		r.Systemd = append(r.Systemd, c)
		if c.Enabled {
			for _, d := range []string{u.WorkingDirectory, appDir(u.ExecPath)} {
				if d != "" && d != "/" {
					workdirs = append(workdirs, workdir{dir: d, unit: u.ID})
				}
			}
			if p := u.stdoutFile(); p != "" {
				r.Files = append(r.Files, Candidate{
					Source:  config.Source{Type: config.TypeFile, Path: p},
					Enabled: true,
					Note:    "сюда пишет stdout юнита " + u.ID,
				})
			}
		}
	}

	if o.Docker != nil {
		cs, err := o.Docker.List(ctx)
		if err != nil {
			r.Warnings = append(r.Warnings, "docker: "+err.Error())
		} else {
			r.Containers = cs
			r.Docker = dockerCandidates(cs)
		}
	}

	r.Files = append(r.Files, scanWorkdirs(workdirs)...)
	r.Files = append(r.Files, scanOpenFiles("/proc")...)
	r.Files = dedupeFiles(r.Files)

	if o.Checker != nil {
		for i := range r.Files {
			r.Files[i].Problem = o.Checker.FileProblem(r.Files[i].Source.Path)
		}
	}

	r.AssignIDs(nil)
	sortCandidates(r.Systemd)
	sortCandidates(r.Files)
	return r
}

func sortCandidates(cs []Candidate) {
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].Enabled != cs[j].Enabled {
			return cs[i].Enabled
		}
		return strings.ToLower(cs[i].Source.ID) < strings.ToLower(cs[j].Source.ID)
	})
}

// ---------------------------------------------------------------- systemd

type unit struct {
	ID               string
	Description      string
	FragmentPath     string
	LoadState        string
	ActiveState      string
	SubState         string
	UnitFileState    string
	WorkingDirectory string
	StandardOutput   string
	ExecPath         string
}

func (u unit) stdoutFile() string {
	for _, pfx := range []string{"append:", "file:", "truncate:"} {
		if p, ok := strings.CutPrefix(u.StandardOutput, pfx); ok && filepath.IsAbs(p) {
			return p
		}
	}
	return ""
}

func listUnits(ctx context.Context, mk logs.CommandFunc) ([]unit, error) {
	names := map[string]bool{}
	for _, args := range [][]string{
		{"list-unit-files", "--type=service", "--no-legend", "--no-pager", "--plain"},
		{"list-units", "--type=service", "--all", "--no-legend", "--no-pager", "--plain"},
	} {
		out, err := mk(ctx, "systemctl", args...).Output()
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(bytes.NewReader(out))
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) == 0 || !strings.HasSuffix(f[0], ".service") || strings.HasSuffix(f[0], "@.service") {
				continue
			}
			names[f[0]] = true
		}
	}
	list := make([]string, 0, len(names))
	for n := range names {
		list = append(list, n)
	}
	sort.Strings(list)

	props := "Id,Description,FragmentPath,LoadState,ActiveState,SubState,UnitFileState,WorkingDirectory,StandardOutput,ExecStart"
	var units []unit
	seen := map[string]bool{}
	for start := 0; start < len(list); start += 100 {
		end := min(start+100, len(list))
		args := append([]string{"show", "--no-pager", "-p", props, "--"}, list[start:end]...)
		out, err := mk(ctx, "systemctl", args...).Output()
		if err != nil {
			return nil, err
		}
		for _, u := range parseShow(out) {
			if u.ID == "" || seen[u.ID] || u.LoadState != "loaded" {
				continue
			}
			seen[u.ID] = true
			units = append(units, u)
		}
	}
	return units, nil
}

var reExecPath = regexp.MustCompile(`path=([^ ;]+)`)

func parseShow(out []byte) []unit {
	var units []unit
	for _, block := range strings.Split(string(out), "\n\n") {
		var u unit
		for _, line := range strings.Split(block, "\n") {
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			switch k {
			case "Id":
				u.ID = v
			case "Description":
				u.Description = v
			case "FragmentPath":
				u.FragmentPath = v
			case "LoadState":
				u.LoadState = v
			case "ActiveState":
				u.ActiveState = v
			case "SubState":
				u.SubState = v
			case "UnitFileState":
				u.UnitFileState = v
			case "WorkingDirectory":
				u.WorkingDirectory = strings.TrimPrefix(v, "!")
			case "StandardOutput":
				u.StandardOutput = v
			case "ExecStart":
				if m := reExecPath.FindStringSubmatch(v); m != nil && u.ExecPath == "" {
					u.ExecPath = m[1]
				}
			}
		}
		if u.ID != "" {
			units = append(units, u)
		}
	}
	return units
}

// Службы, логи которых обычно хотят видеть. Сравнение по имени без .service и @инстанса.
var knownServices = map[string]bool{
	"caddy": true, "nginx": true, "apache2": true, "httpd": true, "haproxy": true, "traefik": true,
	"docker": true, "podman": true,
	"ssh": true, "sshd": true, "tailscaled": true, "wg-quick": true, "openvpn": true, "openvpn-server": true,
	"postgresql": true, "mysql": true, "mariadb": true, "redis-server": true, "redis": true, "mongod": true,
	"fail2ban": true, "cron": true, "crond": true, "cloudflared": true, "unbound": true, "named": true,
	"pihole-FTL": true, "dnsmasq": true, "smbd": true, "jellyfin": true, "plexmediaserver": true,
	"syncthing": true, "grafana-server": true, "prometheus": true, "ollama": true, "mosquitto": true,
	"home-assistant": true, "nextcloud": true, "gitea": true, "forgejo": true,
}

// systemInternal — служебные юниты сессий и консоли: в списке только шумят.
func systemInternal(base string) bool {
	switch base {
	case "user", "user-runtime-dir", "getty", "serial-getty", "autovt":
		return true
	}
	return false
}

var appRoots = []string{"/home/", "/opt/", "/srv/", "/usr/local/"}

func isAppPath(p string) bool {
	for _, r := range appRoots {
		if strings.HasPrefix(p, r) {
			return true
		}
	}
	return false
}

// appDir — каталог своей программы по пути запуска: для ~/bots/x/.venv/bin/python
// это ~/bots/x. Бинарники из /usr/local — системные, их каталог не сканируем.
func appDir(execPath string) string {
	if !isAppPath(execPath) || strings.HasPrefix(execPath, "/usr/local/") {
		return ""
	}
	d := filepath.Dir(execPath)
	if i := strings.Index(d, "/.venv/"); i >= 0 {
		return d[:i]
	}
	if i := strings.Index(d, "/venv/"); i >= 0 {
		return d[:i]
	}
	return d
}

func classifyUnit(u unit) (Candidate, bool) {
	base := strings.TrimSuffix(u.ID, ".service")
	if i := strings.IndexByte(base, '@'); i >= 0 {
		base = base[:i]
	}
	active := u.ActiveState == "active" || u.ActiveState == "activating" || u.ActiveState == "reloading"
	c := Candidate{Source: config.Source{Type: config.TypeSystemd, Unit: u.ID}}
	state := "не запущен"
	if active {
		state = "работает"
	} else if u.ActiveState == "failed" {
		state = "упал"
	}

	custom := strings.HasPrefix(u.FragmentPath, "/etc/systemd/system/") && !strings.HasPrefix(u.ID, "snap.")
	// «active (exited)» — разовые юниты загрузки, логов у них почти нет
	daemon := u.SubState == "running" || u.ActiveState == "failed"
	switch {
	case custom:
		c.Enabled, c.Note = true, "свой юнит, "+state
	case isAppPath(u.ExecPath) && !strings.HasPrefix(u.ID, "snap."):
		c.Enabled, c.Note = true, "запускает "+u.ExecPath+", "+state
	case knownServices[base] && (active || u.UnitFileState == "enabled"):
		c.Enabled, c.Note = true, state
	case daemon && !systemInternal(base):
		c.Note = "системный, " + state
	default:
		return c, false
	}
	if u.Description != "" && u.Description != u.ID {
		c.Note = u.Description + " — " + c.Note
	}
	return c, true
}

// ---------------------------------------------------------------- docker

func dockerCandidates(cs []logs.Container) []Candidate {
	byProject := map[string][]logs.Container{}
	var projects []string
	var out []Candidate
	for _, c := range cs {
		if c.Project == "" {
			out = append(out, containerCandidate(c, ""))
			continue
		}
		if byProject[c.Project] == nil {
			projects = append(projects, c.Project)
		}
		byProject[c.Project] = append(byProject[c.Project], c)
	}
	for _, p := range projects {
		group := byProject[p]
		if len(group) == 1 {
			out = append(out, containerCandidate(group[0], p))
			continue
		}
		names := make([]string, len(group))
		for i, c := range group {
			names[i] = c.Name
		}
		out = append(out, Candidate{
			Source:  config.Source{ID: p, Type: config.TypeDocker, Match: "stack:" + p},
			Enabled: true,
			Note:    "стек: " + strings.Join(names, ", "),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Source.ID < out[j].Source.ID })
	return out
}

func containerCandidate(c logs.Container, project string) Candidate {
	note := c.Image
	if project != "" && project != c.Name {
		note = "стек " + project + ", " + note
	}
	if c.State != "running" {
		note += " (" + c.State + ")"
	}
	return Candidate{
		Source:  config.Source{ID: c.Name, Type: config.TypeDocker, Container: c.Name},
		Enabled: true,
		Note:    note,
	}
}

// ---------------------------------------------------------------- файлы

type workdir struct{ dir, unit string }

var skipDirs = map[string]bool{
	".venv": true, "venv": true, "env": true, "node_modules": true, ".git": true, "__pycache__": true,
	"site-packages": true, ".cache": true, "cache": true, "backups": true, "backup": true,
}

func isLogFile(name string) bool { return strings.HasSuffix(name, ".log") }

func scanWorkdirs(dirs []workdir) []Candidate {
	var out []Candidate
	for _, wd := range dirs {
		root := filepath.Clean(wd.dir)
		found := 0
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return fs.SkipDir
			}
			depth := strings.Count(strings.TrimPrefix(p, root), "/")
			if d.IsDir() {
				if p != root && (skipDirs[d.Name()] || depth > 3) {
					return fs.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() || !isLogFile(d.Name()) || found >= 20 {
				return nil
			}
			found++
			rel := strings.TrimPrefix(p, root+"/")
			// включаем то, что лежит прямо в каталоге программы или в её logs/
			parent := filepath.Base(filepath.Dir(p))
			enabled := !strings.Contains(rel, "/") || (strings.Count(rel, "/") == 1 && (parent == "logs" || parent == "log"))
			out = append(out, Candidate{
				Source:  config.Source{Type: config.TypeFile, Path: p},
				Enabled: enabled,
				Note:    "в каталоге юнита " + wd.unit,
			})
			return nil
		})
	}
	return out
}

var skipOpenPrefixes = []string{"/proc/", "/sys/", "/dev/", "/run/", "/var/lib/docker/", "/var/log/journal/", "/snap/", "/tmp/"}

// scanOpenFiles — *.log, открытые запущенными процессами. От root видны все процессы,
// от обычного пользователя — только свои.
func scanOpenFiles(procRoot string) []Candidate {
	pids, _ := filepath.Glob(filepath.Join(procRoot, "[0-9]*"))
	var out []Candidate
	seen := map[string]bool{}
	for _, pd := range pids {
		fds, err := os.ReadDir(filepath.Join(pd, "fd"))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			t, err := os.Readlink(filepath.Join(pd, "fd", fd.Name()))
			if err != nil || !filepath.IsAbs(t) || !isLogFile(t) || seen[t] {
				continue
			}
			if slicesHasPrefix(skipOpenPrefixes, t) {
				continue
			}
			seen[t] = true
			comm, _ := os.ReadFile(filepath.Join(pd, "comm"))
			out = append(out, Candidate{
				Source: config.Source{Type: config.TypeFile, Path: t},
				Note:   "открыт процессом " + strings.TrimSpace(string(comm)),
			})
		}
	}
	return out
}

func slicesHasPrefix(prefixes []string, s string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// dedupeFiles: один путь — один кандидат; включённость и пояснения объединяются.
func dedupeFiles(cs []Candidate) []Candidate {
	idx := map[string]int{}
	var out []Candidate
	for _, c := range cs {
		p := c.Source.Path
		if i, ok := idx[p]; ok {
			out[i].Enabled = out[i].Enabled || c.Enabled
			if !strings.Contains(out[i].Note, c.Note) {
				out[i].Note += "; " + c.Note
			}
			continue
		}
		idx[p] = len(out)
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Source.Path < out[j].Source.Path })
	return out
}

// ---------------------------------------------------------------- id

var reIDChars = regexp.MustCompile(`[^a-z0-9_.-]+`)

func sanitizeID(s string) string {
	s = reIDChars.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(s, "-.")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-.")
	}
	if s == "" {
		s = "log"
	}
	return s
}

func baseID(c config.Source) string {
	switch c.Type {
	case config.TypeSystemd:
		return strings.TrimSuffix(c.Unit, ".service")
	case config.TypeDocker:
		if c.ID != "" {
			return c.ID
		}
		return c.Container
	}
	name := strings.TrimSuffix(filepath.Base(c.Path), ".log")
	// имя каталога проекта: первый предок, который не logs/log
	dir := filepath.Dir(c.Path)
	for filepath.Base(dir) == "logs" || filepath.Base(dir) == "log" {
		dir = filepath.Dir(dir)
	}
	proj := strings.ToLower(filepath.Base(dir))
	if proj != "" && proj != "/" && proj != "var" && !strings.HasPrefix(strings.ToLower(name), proj) {
		name = proj + "-" + name
	}
	return name
}

// AssignIDs даёт кандидатам уникальные id, не совпадающие с taken (id из конфига).
func (r *Result) AssignIDs(taken map[string]bool) {
	used := map[string]bool{}
	for k := range taken {
		used[k] = true
	}
	for _, group := range [][]Candidate{r.Systemd, r.Docker, r.Files} {
		for i := range group {
			id := sanitizeID(baseID(group[i].Source))
			uniq := id
			for n := 2; used[uniq]; n++ {
				uniq = id + "-" + strconv.Itoa(n)
			}
			used[uniq] = true
			group[i].Source.ID = uniq
		}
	}
}
