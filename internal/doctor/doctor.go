// Package doctor проверяет установку: конфиг, секреты, связь с Telegram и то,
// что пользователь бота действительно может прочитать каждый источник.
package doctor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ReGo2552/tgBot_logViewer/internal/access"
	"github.com/ReGo2552/tgBot_logViewer/internal/config"
	"github.com/ReGo2552/tgBot_logViewer/internal/logs"
	"github.com/ReGo2552/tgBot_logViewer/internal/tgapi"
)

type Status int

const (
	OK Status = iota
	Warn
	Fail
)

func (s Status) Icon() string {
	return [...]string{"✅", "⚠️", "❌"}[s]
}

type Check struct {
	Status Status
	Title  string
	Detail string
	Fix    string
}

type Options struct {
	ConfigPath string
	Config     *config.Config // nil, если не загрузился
	ConfigErr  error
	Env        *config.Env // nil — секреты не проверяем (бот проверяет себя сам)
	Checker    *access.Checker
	Telegram   bool
}

type report struct{ checks []Check }

func (r *report) add(s Status, title, detail, fix string) {
	r.checks = append(r.checks, Check{s, title, detail, fix})
}

func Run(ctx context.Context, o Options) []Check {
	r := &report{}
	cfg := o.Config
	if cfg == nil {
		r.add(Fail, "конфиг "+o.ConfigPath, errText(o.ConfigErr), "sudo logviewer discover --out "+o.ConfigPath)
		return r.checks
	}
	r.add(OK, "конфиг "+o.ConfigPath, fmt.Sprintf("источников: %d", len(cfg.Sources)), "")
	if err := cfg.CheckAdmins(); err != nil {
		r.add(Fail, "admins", err.Error(), "впишите свой Telegram ID в admins")
	}
	if len(cfg.Sources) == 0 {
		r.add(Warn, "sources", "список пуст — показывать нечего", "sudo logviewer discover")
	}

	if o.Env != nil {
		checkEnv(ctx, r, *o.Env, o.Telegram)
	}

	c := o.Checker
	r.add(OK, "пользователь бота", c.Name(), "")
	if cfg.HasType(config.TypeSystemd) {
		checkJournal(ctx, r, cfg, c)
	}
	if cfg.HasType(config.TypeDocker) {
		checkDocker(ctx, r, cfg, c)
	}
	if cfg.HasType(config.TypeFile) {
		checkFiles(r, cfg, c)
	}
	return r.checks
}

func checkEnv(ctx context.Context, r *report, env config.Env, telegram bool) {
	switch {
	case env.Token == "":
		r.add(Fail, "BOT_TOKEN", "не задан", "впишите токен от @BotFather в "+config.EnvPath)
		return
	case !tgapi.ValidToken(env.Token):
		r.add(Fail, "BOT_TOKEN", "не похож на токен бота (123456:ABC…)", "")
		return
	}
	r.add(OK, "BOT_TOKEN", "задан", "")

	client, err := tgapi.NewClient(env.Proxy)
	if err != nil {
		r.add(Fail, "PROXY_URL", err.Error(), "")
		return
	}
	if !telegram {
		return
	}
	via := "напрямую"
	if env.Proxy != "" {
		via = "через прокси"
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	start := time.Now()
	name, err := tgapi.GetMe(ctx, client, env.APIURL, env.Token)
	if err != nil {
		fix := ""
		if env.Proxy == "" {
			fix = "если Telegram заблокирован, задайте PROXY_URL в " + config.EnvPath
		}
		r.add(Fail, "Telegram "+via, tgapi.Scrub(err, env.Token).Error(), fix)
		return
	}
	r.add(OK, "Telegram "+via, fmt.Sprintf("@%s, ответ за %.1f с", name, time.Since(start).Seconds()), "")
}

func checkJournal(ctx context.Context, r *report, cfg *config.Config, c *access.Checker) {
	if c.IsRoot() || c.InGroup("systemd-journal") || c.InGroup("adm") || c.InGroup("wheel") {
		r.add(OK, "journald", "доступ есть", "")
	} else {
		r.add(Fail, "journald", c.Name()+" не в группе systemd-journal — увидит только свои записи",
			"sudo usermod -aG systemd-journal "+c.Name()+" && sudo systemctl restart logviewer")
	}
	for _, s := range cfg.Sources {
		if s.Type != config.TypeSystemd {
			continue
		}
		out, err := logs.DefaultCommand(ctx, "systemctl", "show", "-p", "LoadState", "--value", "--", s.Unit).Output()
		state := strings.TrimSpace(string(out))
		switch {
		case err != nil:
			r.add(Warn, s.ID, "systemctl: "+err.Error(), "")
		case state == "not-found":
			r.add(Warn, s.ID, s.Unit+" не найден — покажутся только старые записи, если они есть", "")
		default:
			r.add(OK, s.ID, s.Unit, "")
		}
	}
}

func checkDocker(ctx context.Context, r *report, cfg *config.Config, c *access.Checker) {
	d := logs.NewDocker(cfg)
	d.Cmd = c.Command
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cs, err := d.List(ctx)
	if err != nil {
		fix := ""
		if d.Sudo {
			fix = "sudo logviewer install  (создаст /etc/sudoers.d/logviewer)"
		}
		r.add(Fail, "docker ("+cfg.Docker.Access+")", err.Error(), fix)
		return
	}
	r.add(OK, "docker ("+cfg.Docker.Access+")", fmt.Sprintf("контейнеров: %d", len(cs)), "")
	for _, s := range cfg.Sources {
		if s.Type != config.TypeDocker {
			continue
		}
		var names []string
		for _, ct := range cs {
			if logs.Matches(s, ct) {
				names = append(names, ct.Name)
			}
		}
		if len(names) == 0 {
			r.add(Warn, s.ID, "нет подходящих контейнеров", "")
		} else {
			r.add(OK, s.ID, strings.Join(names, ", "), "")
		}
	}
}

func checkFiles(r *report, cfg *config.Config, c *access.Checker) {
	for _, s := range cfg.Sources {
		if s.Type != config.TypeFile {
			continue
		}
		files, err := logs.ResolveFiles(s)
		if err != nil || len(files) == 0 {
			r.add(Warn, s.ID, "по пути "+s.Path+" файлов нет", "")
			continue
		}
		bad := 0
		for i, f := range files {
			if i >= 5 {
				break
			}
			if p := c.FileProblem(f); p != nil {
				r.add(Fail, s.ID, p.What, p.Fix)
				bad++
			}
		}
		if bad == 0 {
			detail := files[0]
			if len(files) > 1 {
				detail = fmt.Sprintf("%d файлов, %s…", len(files), files[0])
			}
			r.add(OK, s.ID, detail, "")
		}
	}
}

// Format — текстовый отчёт; одинаковый для консоли и бота.
func Format(checks []Check) string {
	var b strings.Builder
	fails := 0
	for _, c := range checks {
		fmt.Fprintf(&b, "%s %s", c.Status.Icon(), c.Title)
		if c.Detail != "" {
			fmt.Fprintf(&b, ": %s", c.Detail)
		}
		b.WriteByte('\n')
		if c.Fix != "" {
			fmt.Fprintf(&b, "   → %s\n", c.Fix)
		}
		if c.Status == Fail {
			fails++
		}
	}
	if fails == 0 {
		b.WriteString("\nВсё в порядке.\n")
	} else {
		fmt.Fprintf(&b, "\nПроблем: %d.\n", fails)
	}
	return b.String()
}

func Failed(checks []Check) bool {
	for _, c := range checks {
		if c.Status == Fail {
			return true
		}
	}
	return false
}

func errText(err error) string {
	if err == nil {
		return "не загружен"
	}
	return err.Error()
}
