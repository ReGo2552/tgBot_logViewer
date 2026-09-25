// Package config — конфиг logviewer: кому отвечать и какие логи показывать.
//
// Бот показывает только источники, перечисленные в конфиге. Пользователь в чате
// выбирает источник из списка и никогда не передаёт имя юнита, контейнера или путь.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata" // часовые пояса без зависимости от tzdata на сервере

	"go.yaml.in/yaml/v3"
)

// Пути установки. BinaryPath зашит в sudoers, поэтому это константа, а не флаг.
const (
	Dir         = "/etc/log-viewer"
	DefaultPath = Dir + "/config.yaml"
	EnvPath     = Dir + "/.env"
	BinaryPath  = "/usr/local/bin/logviewer"
)

const (
	TypeSystemd = "systemd"
	TypeDocker  = "docker"
	TypeFile    = "file"
)

const (
	DockerSudo   = "sudo"
	DockerDirect = "direct"
)

type Config struct {
	Admins   []int64  `yaml:"admins"`
	Timezone string   `yaml:"timezone"`
	Lines    Lines    `yaml:"lines"`
	Docker   Docker   `yaml:"docker"`
	Redact   Redact   `yaml:"redact"`
	Sources  []Source `yaml:"sources"`

	Location *time.Location `yaml:"-"`
}

type Lines struct {
	Default      int `yaml:"default"`
	Max          int `yaml:"max"`
	FilterWindow int `yaml:"filter_window"`
}

type Docker struct {
	Binary string `yaml:"binary"`
	Access string `yaml:"access"`
}

type Redact struct {
	Disable bool     `yaml:"disable"`
	Extra   []string `yaml:"extra"`
}

type Source struct {
	ID        string `yaml:"id"`
	Type      string `yaml:"type"`
	Title     string `yaml:"title,omitempty"`
	Unit      string `yaml:"unit,omitempty"`
	Container string `yaml:"container,omitempty"`
	Match     string `yaml:"match,omitempty"`
	Path      string `yaml:"path,omitempty"`
}

// Label — как источник подписан в боте.
func (s Source) Label() string {
	if s.Title != "" {
		return s.Title
	}
	return s.ID
}

// IsMulti — источник раскрывается в несколько целей (стек контейнеров, glob файлов).
func (s Source) IsMulti() bool {
	switch s.Type {
	case TypeDocker:
		return s.Match != ""
	case TypeFile:
		return HasGlob(s.Path)
	}
	return false
}

var (
	reID        = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,40}$`)
	reUnit      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@:._\\-]*$`)
	reContainer = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	reStack     = regexp.MustCompile(`^stack:[A-Za-z0-9][A-Za-z0-9_.-]*$`)
)

// ValidContainerName — имя, которое безопасно передать docker как аргумент.
func ValidContainerName(s string) bool { return len(s) <= 128 && reContainer.MatchString(s) }

func HasGlob(p string) bool { return strings.ContainsAny(p, "*?[") }

func Load(p string) (*Config, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	c, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return c, nil
}

func Parse(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return &c, nil
}

// CheckAdmins — отдельно от normalize: помощнику docker и discover админы не нужны.
func (c *Config) CheckAdmins() error {
	if len(c.Admins) == 0 {
		return errors.New("admins пуст: бот никому не будет отвечать")
	}
	for _, id := range c.Admins {
		if id <= 0 {
			return fmt.Errorf("admins: %d — не Telegram ID (узнать свой: @userinfobot)", id)
		}
	}
	return nil
}

func (c *Config) IsAdmin(id int64) bool {
	for _, a := range c.Admins {
		if a == id {
			return true
		}
	}
	return false
}

func (c *Config) Source(id string) (Source, bool) {
	for _, s := range c.Sources {
		if s.ID == id {
			return s, true
		}
	}
	return Source{}, false
}

func (c *Config) HasType(t string) bool {
	for _, s := range c.Sources {
		if s.Type == t {
			return true
		}
	}
	return false
}

// ClampLines приводит запрошенное число строк к [1, max]; 0 — значение по умолчанию.
func (c *Config) ClampLines(n int) int {
	switch {
	case n <= 0:
		return c.Lines.Default
	case n > c.Lines.Max:
		return c.Lines.Max
	}
	return n
}

func (c *Config) normalize() error {
	if c.Lines.Default == 0 {
		c.Lines.Default = 100
	}
	if c.Lines.Max == 0 {
		c.Lines.Max = 1000
	}
	if c.Lines.FilterWindow == 0 {
		c.Lines.FilterWindow = 5000
	}
	if c.Lines.Default < 1 || c.Lines.Max < c.Lines.Default || c.Lines.Max > 20000 {
		return errors.New("lines: нужно 1 <= default <= max <= 20000")
	}
	if c.Lines.FilterWindow < c.Lines.Max || c.Lines.FilterWindow > 100000 {
		return errors.New("lines: нужно max <= filter_window <= 100000")
	}

	if c.Docker.Binary == "" {
		c.Docker.Binary = "docker"
	}
	if c.Docker.Access == "" {
		c.Docker.Access = DockerSudo
	}
	if c.Docker.Access != DockerSudo && c.Docker.Access != DockerDirect {
		return fmt.Errorf("docker.access: %q, ожидается sudo или direct", c.Docker.Access)
	}
	if strings.HasPrefix(c.Docker.Binary, "-") {
		return fmt.Errorf("docker.binary: %q", c.Docker.Binary)
	}

	c.Location = time.Local
	if c.Timezone != "" {
		loc, err := time.LoadLocation(c.Timezone)
		if err != nil {
			return fmt.Errorf("timezone: %w", err)
		}
		c.Location = loc
	}

	for _, re := range c.Redact.Extra {
		if _, err := regexp.Compile(re); err != nil {
			return fmt.Errorf("redact.extra: %q: %w", re, err)
		}
	}

	seen := map[string]bool{}
	for i := range c.Sources {
		s := &c.Sources[i]
		if err := s.normalize(); err != nil {
			name := s.ID
			if name == "" {
				name = fmt.Sprintf("#%d", i+1)
			}
			return fmt.Errorf("sources %s: %w", name, err)
		}
		if seen[s.ID] {
			return fmt.Errorf("sources: id %q повторяется", s.ID)
		}
		seen[s.ID] = true
	}
	return nil
}

func (s *Source) normalize() error {
	if !reID.MatchString(s.ID) {
		return errors.New("id: 1–40 символов из A-Z a-z 0-9 _ . -")
	}
	set := 0
	for _, v := range []string{s.Unit, s.Container, s.Match, s.Path} {
		if v != "" {
			set++
		}
	}
	if set != 1 {
		return errors.New("нужно ровно одно из полей unit, container, match, path")
	}
	switch s.Type {
	case TypeSystemd:
		if s.Unit == "" {
			return errors.New("для systemd нужен unit")
		}
		if !reUnit.MatchString(s.Unit) {
			return fmt.Errorf("unit %q: недопустимые символы (маски не поддерживаются)", s.Unit)
		}
		if !strings.Contains(s.Unit, ".") {
			s.Unit += ".service"
		}
	case TypeDocker:
		switch {
		case s.Container != "":
			if !ValidContainerName(s.Container) {
				return fmt.Errorf("container %q: недопустимое имя", s.Container)
			}
		case s.Match != "":
			if strings.HasPrefix(s.Match, "stack:") {
				if !reStack.MatchString(s.Match) {
					return fmt.Errorf("match %q: после stack: нужно имя compose-проекта", s.Match)
				}
			} else if _, err := path.Match(s.Match, ""); err != nil {
				return fmt.Errorf("match %q: %w", s.Match, err)
			}
		default:
			return errors.New("для docker нужен container или match")
		}
	case TypeFile:
		if s.Path == "" {
			return errors.New("для file нужен path")
		}
		if !filepath.IsAbs(s.Path) || filepath.Clean(s.Path) != s.Path {
			return fmt.Errorf("path %q: нужен абсолютный путь без ./ и ..", s.Path)
		}
		if _, err := filepath.Match(s.Path, ""); err != nil {
			return fmt.Errorf("path %q: %w", s.Path, err)
		}
	case "":
		return errors.New("не указан type (systemd, docker, file)")
	default:
		return fmt.Errorf("type %q: ожидается systemd, docker или file", s.Type)
	}
	return nil
}
