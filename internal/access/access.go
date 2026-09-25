// Package access отвечает на вопрос «сможет ли пользователь бота это прочитать»
// и подсказывает команду, которая это исправит.
//
// Проверять можно за себя (бот проверяет сам себя) или, будучи root, за другого
// пользователя: тогда проверки выполняются дочерним процессом с его uid/gid/группами,
// и ответ даёт само ядро, включая ACL.
package access

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	R = 4
	X = 1
)

type Checker struct {
	name   string
	uid    uint32
	gid    uint32
	groups []uint32
	self   bool
}

func Self() (*Checker, error) {
	u, err := user.Current()
	if err != nil {
		return nil, err
	}
	c, err := fromUser(u)
	if err != nil {
		return nil, err
	}
	c.self = true
	return c, nil
}

// As — проверять за пользователя name. Нужен root, если это не текущий пользователь.
func As(name string) (*Checker, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return nil, err
	}
	c, err := fromUser(u)
	if err != nil {
		return nil, err
	}
	if int(c.uid) == os.Getuid() {
		c.self = true
	} else if os.Geteuid() != 0 {
		return nil, fmt.Errorf("проверять права за %s можно только от root", name)
	}
	return c, nil
}

func fromUser(u *user.User) (*Checker, error) {
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, err
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, err
	}
	c := &Checker{name: u.Username, uid: uint32(uid), gid: uint32(gid)}
	gids, err := u.GroupIds()
	if err != nil {
		return nil, err
	}
	for _, g := range gids {
		if v, err := strconv.ParseUint(g, 10, 32); err == nil {
			c.groups = append(c.groups, uint32(v))
		}
	}
	return c, nil
}

func (c *Checker) Name() string { return c.name }
func (c *Checker) IsRoot() bool { return c.uid == 0 }

// InGroup — состоит ли пользователь в группе. Для себя смотрим группы процесса:
// после usermod они меняются только у новых процессов.
func (c *Checker) InGroup(name string) bool {
	g, err := user.LookupGroup(name)
	if err != nil {
		return false
	}
	gid, _ := strconv.ParseUint(g.Gid, 10, 32)
	if c.self {
		gs, _ := os.Getgroups()
		return slices.Contains(gs, int(gid)) || os.Getgid() == int(gid)
	}
	return c.gid == uint32(gid) || slices.Contains(c.groups, uint32(gid))
}

// Command — команда, выполняемая от имени проверяемого пользователя.
func (c *Checker) Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "SYSTEMD_COLORS=0", "SYSTEMD_PAGER=", "LC_ALL=C.UTF-8")
	cmd.WaitDelay = 2 * time.Second
	if !c.self {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
			Uid: c.uid, Gid: c.gid, Groups: c.groups,
		}}
		cmd.Env = append(cmd.Env, "HOME=/", "USER="+c.name, "LOGNAME="+c.name)
		cmd.Dir = "/"
	}
	return cmd
}

func (c *Checker) Can(path string, mode uint32) bool {
	if c.self {
		return syscall.Access(path, mode) == nil
	}
	flag := "-r"
	if mode == X {
		flag = "-x"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.Command(ctx, "test", flag, path).Run() == nil
}

// Problem — почему файл не читается и как это исправить.
type Problem struct {
	What string
	Fix  string
}

// FileProblem проверяет весь путь: право прохода (x) по каталогам и чтения (r) файла.
// nil — всё читается.
func (c *Checker) FileProblem(path string) *Problem {
	// Отказ в доступе — не повод остановиться: ниже найдём, какой каталог не пускает.
	st, err := os.Stat(path)
	switch {
	case err != nil && !errors.Is(err, fs.ErrPermission):
		return &Problem{What: fmt.Sprintf("файл недоступен: %v", err)}
	case err == nil && st.IsDir():
		return &Problem{What: path + " — это каталог, а не файл"}
	}
	var dirs []string
	for d := filepath.Dir(path); ; d = filepath.Dir(d) {
		dirs = append(dirs, d)
		if d == "/" {
			break
		}
	}
	slices.Reverse(dirs)
	for _, d := range dirs {
		if !c.Can(d, X) {
			return &Problem{
				What: fmt.Sprintf("%s не может войти в каталог %s", c.name, d),
				Fix:  fmt.Sprintf("sudo setfacl -m u:%s:x %s", c.name, shellQuote(d)),
			}
		}
	}
	if !c.Can(path, R) {
		dir := filepath.Dir(path)
		return &Problem{
			What: fmt.Sprintf("%s не может прочитать %s", c.name, path),
			// default ACL — чтобы права пережили ротацию (новый файл создаётся заново)
			Fix: fmt.Sprintf("sudo setfacl -m u:%[1]s:r %[2]s && sudo setfacl -d -m u:%[1]s:r %[3]s",
				c.name, shellQuote(path), shellQuote(dir)),
		}
	}
	return nil
}

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r == '/' || r == '.' || r == '_' || r == '-' || r == '@' || r == '+' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
