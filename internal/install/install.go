// Package install ставит logviewer на сервер: пользователь, права, конфиг, юнит.
// Повторный запуск обновляет бинарник, юнит и sudoers, но не трогает конфиг и .env.
package install

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ReGo2552/tgBot_logViewer/internal/access"
	"github.com/ReGo2552/tgBot_logViewer/internal/config"
	"github.com/ReGo2552/tgBot_logViewer/internal/discover"
	"github.com/ReGo2552/tgBot_logViewer/internal/doctor"
	"github.com/ReGo2552/tgBot_logViewer/internal/logs"
)

type Options struct {
	User     string
	Admin    int64
	Timezone string
	In       io.Reader
	Out      io.Writer
}

type installer struct {
	Options
	step, steps int
}

func Run(ctx context.Context, o Options) error {
	in := &installer{Options: o, steps: 8}
	if os.Geteuid() != 0 {
		return errors.New("нужен root: sudo logviewer install")
	}
	if o.Timezone != "" {
		if _, err := time.LoadLocation(o.Timezone); err != nil {
			return fmt.Errorf("--timezone: %w", err)
		}
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return errors.New("systemd не найден: поддерживаются только системы с systemd")
	}
	if _, err := os.Stat("/etc/debian_version"); err != nil {
		in.say("⚠ не Debian/Ubuntu: проверялось только там, продолжаю")
	}

	in.next("пользователь " + o.User)
	if err := in.ensureUser(ctx); err != nil {
		return err
	}

	in.next("бинарник " + config.BinaryPath)
	if err := installBinary(); err != nil {
		return err
	}

	in.next("каталог " + config.Dir)
	gid, err := groupID(o.User)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(config.Dir, 0o750); err != nil {
		return err
	}
	if err := chownMode(config.Dir, 0, gid, 0o750); err != nil {
		return err
	}

	in.next("конфиг " + config.DefaultPath)
	cfg, err := in.ensureConfig(ctx, gid)
	if err != nil {
		return err
	}

	in.next("секреты " + config.EnvPath)
	if err := in.ensureEnv(); err != nil {
		return err
	}

	in.next("sudo-правило для docker")
	needSudo := cfg.HasType(config.TypeDocker) && cfg.Docker.Access == config.DockerSudo
	if needSudo {
		if err := in.installSudoers(ctx); err != nil {
			return err
		}
	} else {
		in.say("не нужно (нет docker-источников или docker.access: direct)")
	}

	in.next("systemd-юнит " + UnitPath)
	if err := writeFile(UnitPath, []byte(Unit(o.User, needSudo)), 0, 0, 0o644); err != nil {
		return err
	}
	if err := in.run(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := in.run(ctx, "systemctl", "enable", "logviewer.service"); err != nil {
		return err
	}
	_ = config.LoadEnvFile(config.EnvPath)
	env := config.EnvFromOS()

	in.next("проверка (doctor от имени " + o.User + ")")
	checker, err := access.As(o.User)
	if err != nil {
		return err
	}
	checks := doctor.Run(ctx, doctor.Options{
		ConfigPath: config.DefaultPath, Config: cfg, Env: &env, Checker: checker, Telegram: env.Token != "",
	})
	fmt.Fprint(in.Out, doctor.Format(checks))

	fmt.Fprintln(in.Out)
	switch {
	case env.Token == "" || cfg.CheckAdmins() != nil:
		in.say("Дальше:")
		if env.Token == "" {
			in.say("  1. впишите BOT_TOKEN (и PROXY_URL, если нужен) в " + config.EnvPath)
		}
		if cfg.CheckAdmins() != nil {
			in.say("  2. впишите свой Telegram ID в admins в " + config.DefaultPath)
		}
		in.say("  3. проверьте источники в " + config.DefaultPath + " и выполните: sudo logviewer doctor")
		in.say("  4. sudo systemctl start logviewer")
	case doctor.Failed(checks):
		in.say("Бот не запущен: сначала исправьте ❌ выше, затем sudo systemctl restart logviewer")
	default:
		if err := in.run(ctx, "systemctl", "restart", "logviewer.service"); err != nil {
			return err
		}
		in.say("Бот запущен. Логи самого бота: journalctl -u logviewer -f")
	}
	return nil
}

func (in *installer) next(title string) {
	in.step++
	fmt.Fprintf(in.Out, "\n[%d/%d] %s\n", in.step, in.steps, title)
}

func (in *installer) say(s string) { fmt.Fprintln(in.Out, "    "+s) }

func (in *installer) run(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v\n%s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (in *installer) ensureUser(ctx context.Context) error {
	if _, err := user.Lookup(in.User); err == nil {
		in.say("уже есть")
	} else {
		err := in.run(ctx, "useradd", "--system", "--user-group", "--no-create-home",
			"--home-dir", "/nonexistent", "--shell", "/usr/sbin/nologin", "--comment", "logviewer bot", in.User)
		if err != nil {
			return err
		}
		in.say("создан системный пользователь без shell и домашнего каталога")
	}
	// systemd-journal — читать журнал целиком. adm сознательно не даём: он открывает
	// /var/log/auth.log и syslog, а они для бота не нужны.
	if _, err := user.LookupGroup("systemd-journal"); err == nil {
		if err := in.run(ctx, "usermod", "--append", "--groups", "systemd-journal", in.User); err != nil {
			return err
		}
		in.say("в группе systemd-journal (чтение журнала)")
	}
	return nil
}

func installBinary() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	self, _ = filepath.EvalSymlinks(self)
	if self == config.BinaryPath {
		return nil
	}
	data, err := os.ReadFile(self)
	if err != nil {
		return err
	}
	return writeFile(config.BinaryPath, data, 0, 0, 0o755)
}

func (in *installer) ensureConfig(ctx context.Context, gid int) (*config.Config, error) {
	if _, err := os.Stat(config.DefaultPath); err == nil {
		cfg, err := config.Load(config.DefaultPath)
		if err != nil {
			return nil, fmt.Errorf("существующий конфиг не читается, исправьте его: %w", err)
		}
		if err := chownMode(config.DefaultPath, 0, gid, 0o640); err != nil {
			return nil, err
		}
		in.say("уже есть, не трогаю (новые источники: sudo logviewer discover)")
		return cfg, nil
	}

	admin := in.Admin
	if admin == 0 {
		admin = in.askAdmin()
	}
	checker, err := access.As(in.User)
	if err != nil {
		return nil, err
	}
	opts := discover.Options{Checker: checker, Self: "logviewer.service"}
	if _, err := exec.LookPath("docker"); err == nil {
		opts.Docker = &logs.Docker{Binary: "docker", Cmd: logs.DefaultCommand}
	}
	sctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	res := discover.Scan(sctx, opts)
	host, _ := os.Hostname()
	meta := discover.Meta{Host: host, User: in.User, Timezone: in.Timezone, Generated: time.Now()}
	if admin != 0 {
		meta.Admins = []int64{admin}
	}
	text := discover.RenderConfig(res, meta)
	cfg, err := config.Parse([]byte(text))
	if err != nil {
		return nil, fmt.Errorf("черновик конфига не прошёл проверку (это ошибка logviewer): %w", err)
	}
	if err := writeFile(config.DefaultPath, []byte(text), 0, gid, 0o640); err != nil {
		return nil, err
	}
	on := 0
	for _, c := range res.All() {
		if c.Enabled {
			on++
		}
	}
	in.say(fmt.Sprintf("создан черновик: включено %d источников, ещё %d закомментировано — просмотрите файл",
		on, len(res.All())-on))
	for _, w := range res.Warnings {
		in.say("⚠ " + w)
	}
	return cfg, nil
}

func (in *installer) askAdmin() int64 {
	if in.In == nil {
		return 0
	}
	fmt.Fprint(in.Out, "    Ваш Telegram ID (узнать: @userinfobot; Enter — пропустить): ")
	line, _ := bufio.NewReader(in.In).ReadString('\n')
	id, err := strconv.ParseInt(strings.TrimSpace(line), 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

func (in *installer) ensureEnv() error {
	if _, err := os.Stat(config.EnvPath); err == nil {
		if err := chownMode(config.EnvPath, 0, 0, 0o600); err != nil {
			return err
		}
		in.say("уже есть, не трогаю")
		return nil
	}
	if err := writeFile(config.EnvPath, []byte(envTemplate), 0, 0, 0o600); err != nil {
		return err
	}
	in.say("создан шаблон, впишите BOT_TOKEN")
	return nil
}

// installSudoers: файл с точкой в имени sudo игнорирует, поэтому временный
// logviewer.tmp безопасен, пока visudo его не одобрил.
func (in *installer) installSudoers(ctx context.Context) error {
	tmp := SudoersPath + ".tmp"
	if err := writeFile(tmp, []byte(Sudoers(in.User)), 0, 0, 0o440); err != nil {
		return err
	}
	if err := in.run(ctx, "visudo", "-c", "-q", "-f", tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, SudoersPath); err != nil {
		return err
	}
	in.say(SudoersPath + ": " + in.User + " → только `logviewer docker-helper`")
	return nil
}

// writeFile пишет атомарно: во временный файл рядом и переименованием.
func writeFile(path string, data []byte, uid, gid int, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := chownMode(tmp.Name(), uid, gid, mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func chownMode(path string, uid, gid int, mode os.FileMode) error {
	if err := os.Chown(path, uid, gid); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

func groupID(name string) (int, error) {
	g, err := user.LookupGroup(name)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(g.Gid)
}
