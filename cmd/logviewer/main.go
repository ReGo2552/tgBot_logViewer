// logviewer — Telegram-бот, который по запросу показывает последние строки логов
// systemd-служб, docker-контейнеров и файлов.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"os/user"
	"strconv"
	"syscall"
	"time"

	"github.com/ReGo2552/tgBot_logViewer/internal/access"
	"github.com/ReGo2552/tgBot_logViewer/internal/config"
	"github.com/ReGo2552/tgBot_logViewer/internal/discover"
	"github.com/ReGo2552/tgBot_logViewer/internal/doctor"
	"github.com/ReGo2552/tgBot_logViewer/internal/install"
	"github.com/ReGo2552/tgBot_logViewer/internal/logs"
	"github.com/ReGo2552/tgBot_logViewer/internal/redact"
	"github.com/ReGo2552/tgBot_logViewer/internal/tgbot"
)

var version = "dev"

const botUser = "logbot"

const usage = `logviewer %s — логи сервера в Telegram

  sudo logviewer install [--admin ID] [--timezone TZ]
                                        установить: пользователь, конфиг, юнит, sudo-правило
  sudo logviewer discover [--out FILE]  найти источники и напечатать черновик конфига
  sudo logviewer doctor                 проверить конфиг, токен, прокси и доступ к логам
  logviewer show ID [N] [ФИЛЬТР]        напечатать то, что бот покажет в чате
  logviewer run [--config FILE]         запустить бота (так его запускает systemd)
  logviewer version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usage, version)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "run":
		err = cmdRun(ctx, args)
	case "discover":
		err = cmdDiscover(ctx, args)
	case "doctor":
		err = cmdDoctor(ctx, args)
	case "install":
		err = cmdInstall(ctx, args)
	case "show":
		err = cmdShow(ctx, args)
	case "docker-helper":
		err = cmdDockerHelper(ctx, args)
	case "version", "--version", "-v":
		fmt.Println(version)
	case "help", "--help", "-h":
		fmt.Printf(usage, version)
	default:
		fmt.Fprintf(os.Stderr, usage, version)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
}

func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "путь к config.yaml")
	envPath := fs.String("env", "", "файл с BOT_TOKEN/PROXY_URL (в сервисе их передаёт systemd)")
	fs.Parse(args)

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		// время ставит journald
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
	if *envPath != "" {
		if err := config.LoadEnvFile(*envPath); err != nil {
			return err
		}
	}
	env := config.EnvFromOS()
	if env.Token == "" {
		return errors.New("BOT_TOKEN не задан (" + config.EnvPath + ")")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if err := cfg.CheckAdmins(); err != nil {
		return err
	}
	fetcher, err := newFetcher(cfg)
	if err != nil {
		return err
	}
	return tgbot.Run(ctx, tgbot.Options{
		Config:  cfg,
		Env:     env,
		Fetcher: fetcher,
		Log:     log,
		Doctor: func(ctx context.Context) string {
			checker, err := access.Self()
			if err != nil {
				return err.Error()
			}
			return doctor.Format(doctor.Run(ctx, doctor.Options{
				ConfigPath: *cfgPath, Config: cfg, Checker: checker,
			}))
		},
		Discover: func(ctx context.Context) (string, error) {
			checker, err := access.Self()
			if err != nil {
				return "", err
			}
			res := discover.Scan(ctx, discover.Options{
				Docker: fetcher.Docker, Checker: checker, Self: "logviewer.service",
			})
			miss := discover.Missing(cfg, res)
			all := miss.All()
			if len(all) == 0 {
				text := "Всё найденное уже есть в конфиге.\n"
				for _, w := range res.Warnings {
					text += "⚠ " + w + "\n"
				}
				text += "\nПолный список с системными службами: sudo logviewer discover"
				return text, nil
			}
			text := "# Нет в конфиге. Добавьте нужное в sources:\n# " + *cfgPath +
				"\n# и перезапустите: sudo systemctl restart logviewer\n\n" + discover.RenderSnippet(all)
			for _, w := range res.Warnings {
				text += "\n# ⚠ " + w
			}
			return text, nil
		},
	})
}

// cmdShow — отладка без Telegram: тот же конвейер (чтение, фильтр, маскирование).
func cmdShow(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "путь к config.yaml")
	target := fs.String("target", "", "контейнер стека или файл из glob")
	fs.Parse(args)
	if fs.NArg() == 0 {
		return errors.New("использование: logviewer show [--config FILE] [--target T] ID [N] [ФИЛЬТР]")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	s, ok := cfg.Source(fs.Arg(0))
	if !ok {
		return fmt.Errorf("источника %q нет в конфиге", fs.Arg(0))
	}
	req := logs.Request{Source: s, Target: *target}
	rest := fs.Args()[1:]
	if len(rest) > 0 {
		if n, err := strconv.Atoi(rest[0]); err == nil {
			req.Lines, rest = n, rest[1:]
		}
	}
	for i, w := range rest {
		if i > 0 {
			req.Filter += " "
		}
		req.Filter += w
	}
	fetcher, err := newFetcher(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if s.IsMulti() && req.Target == "" {
		ts, err := fetcher.Targets(ctx, s)
		if err != nil {
			return err
		}
		if len(ts) != 1 {
			fmt.Printf("у %s %d целей, выберите --target:\n", s.ID, len(ts))
			for _, t := range ts {
				fmt.Println("  " + t.Name)
			}
			return nil
		}
	}
	res, err := fetcher.Fetch(ctx, req)
	if err != nil {
		return err
	}
	for _, l := range res.Lines {
		if !l.Time.IsZero() {
			fmt.Print(l.Time.In(cfg.Location).Format("01-02 15:04:05"), " ")
		}
		fmt.Println(l.Text)
	}
	fmt.Fprintf(os.Stderr, "— %s: %d строк (просмотрено %d)\n", res.Target, len(res.Lines), res.Scanned)
	return nil
}

func newFetcher(cfg *config.Config) (*logs.Fetcher, error) {
	red, err := redact.New(cfg.Redact.Disable, cfg.Redact.Extra)
	if err != nil {
		return nil, err
	}
	return &logs.Fetcher{Cfg: cfg, Docker: logs.NewDocker(cfg), Redactor: red, Cmd: logs.DefaultCommand}, nil
}

// checkerFor: от root проверяем права за пользователя бота, иначе — за себя.
func checkerFor(name string) (*access.Checker, error) {
	if name == "" {
		if os.Geteuid() == 0 {
			if _, err := user.Lookup(botUser); err == nil {
				return access.As(botUser)
			}
		}
		return access.Self()
	}
	return access.As(name)
}

func cmdDiscover(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("discover", flag.ExitOnError)
	out := fs.String("out", "", "записать в файл (по умолчанию — в stdout); существующий не перезаписывается")
	admin := fs.Int64("admin", 0, "Telegram ID для admins")
	as := fs.String("as", "", "проверять права за этого пользователя (по умолчанию "+botUser+", если он есть)")
	tz := fs.String("timezone", "", "часовой пояс, например Europe/Moscow")
	fs.Parse(args)

	checker, err := checkerFor(*as)
	if err != nil {
		return err
	}
	opts := discover.Options{Checker: checker, Self: "logviewer.service"}
	// docker: напрямую, если можем (root), иначе через sudo-помощника
	if cfg, err := config.Load(config.DefaultPath); err == nil && os.Geteuid() != 0 {
		opts.Docker = logs.NewDocker(cfg)
	} else {
		opts.Docker = &logs.Docker{Binary: "docker", Cmd: logs.DefaultCommand}
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "подсказка: без sudo видны не все процессы и файлы")
	}
	res := discover.Scan(ctx, opts)
	host, _ := os.Hostname()
	meta := discover.Meta{Host: host, User: checker.Name(), Timezone: *tz, Generated: time.Now()}
	if *admin != 0 {
		meta.Admins = []int64{*admin}
	}
	text := discover.RenderConfig(res, meta)
	if _, err := config.Parse([]byte(text)); err != nil {
		return fmt.Errorf("черновик не прошёл проверку (это ошибка logviewer): %w", err)
	}
	if *out == "" {
		fmt.Print(text)
		return nil
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(text)
	return err
}

func cmdDoctor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "путь к config.yaml")
	envPath := fs.String("env", config.EnvPath, "файл с BOT_TOKEN/PROXY_URL")
	as := fs.String("as", "", "проверять за этого пользователя (по умолчанию "+botUser+", если он есть)")
	fs.Parse(args)

	if err := config.LoadEnvFile(*envPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "⚠️ %s: %v\n", *envPath, err)
	}
	env := config.EnvFromOS()
	checker, err := checkerFor(*as)
	if err != nil {
		return err
	}
	cfg, cfgErr := config.Load(*cfgPath)
	checks := doctor.Run(ctx, doctor.Options{
		ConfigPath: *cfgPath, Config: cfg, ConfigErr: cfgErr, Env: &env, Checker: checker, Telegram: true,
	})
	fmt.Print(doctor.Format(checks))
	if doctor.Failed(checks) {
		os.Exit(1)
	}
	return nil
}

func cmdInstall(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	admin := fs.Int64("admin", 0, "ваш Telegram ID (иначе спросит)")
	tz := fs.String("timezone", "", "часовой пояс для времени в логах, например Europe/Moscow")
	fs.Parse(args)
	return install.Run(ctx, install.Options{
		User: botUser, Admin: *admin, Timezone: *tz, In: os.Stdin, Out: os.Stdout,
	})
}

// cmdDockerHelper запускается от root через sudo (правило в /etc/sudoers.d/logviewer).
// Аргументы приходят от непривилегированного бота, поэтому всё проверяется здесь:
// конфиг — только из /etc/log-viewer и только если его не может подменить не-root,
// контейнер — только из разрешённых конфигом.
func cmdDockerHelper(ctx context.Context, args []string) error {
	if os.Geteuid() != 0 {
		return errors.New("docker-helper запускается только через sudo")
	}
	if err := trustedPath(config.Dir); err != nil {
		return err
	}
	if err := trustedPath(config.DefaultPath); err != nil {
		return err
	}
	cfg, err := config.Load(config.DefaultPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	d := &logs.Docker{Binary: cfg.Docker.Binary, Cmd: logs.DefaultCommand}
	enc := json.NewEncoder(os.Stdout)

	switch {
	case len(args) == 1 && args[0] == "ps":
		cs, err := d.List(ctx)
		if err != nil {
			return err
		}
		return enc.Encode(cs)
	case len(args) == 3 && args[0] == "logs":
		name := args[1]
		n, err := strconv.Atoi(args[2])
		if err != nil || n < 1 || n > max(cfg.Lines.Max, cfg.Lines.FilterWindow) {
			return fmt.Errorf("недопустимое число строк %q", args[2])
		}
		if !config.ValidContainerName(name) {
			return fmt.Errorf("недопустимое имя контейнера %q", name)
		}
		cs, err := d.List(ctx)
		if err != nil {
			return err
		}
		allowed := false
		for _, c := range cs {
			if c.Name == name && logs.AllowedBy(cfg, c) {
				allowed = true
			}
		}
		if !allowed {
			return fmt.Errorf("контейнер %q не разрешён конфигом", name)
		}
		lines, err := d.Logs(ctx, name, n)
		if err != nil {
			return err
		}
		return enc.Encode(lines)
	}
	return errors.New("использование: docker-helper ps | docker-helper logs КОНТЕЙНЕР N")
}

// trustedPath — владелец root и запись только для root.
func trustedPath(p string) error {
	st, err := os.Stat(p)
	if err != nil {
		return err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || sys.Uid != 0 || st.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s должен принадлежать root и быть закрыт на запись для остальных", p)
	}
	return nil
}
