package config

import (
	"os"
	"testing"
)

func TestParseValid(t *testing.T) {
	c, err := Parse([]byte(`
admins: [123]
timezone: Asia/Novosibirsk
sources:
  - {id: caddy, type: systemd, unit: caddy}
  - {id: immich, type: docker, match: "stack:immich"}
  - {id: kuma, type: docker, container: uptime-kuma}
  - {id: web, type: docker, match: "web_*"}
  - {id: mc, type: file, path: /srv/minecraft/logs/latest.log}
  - {id: nginx, type: file, path: "/var/log/nginx/*.log"}
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CheckAdmins(); err != nil {
		t.Fatal(err)
	}
	if s, _ := c.Source("caddy"); s.Unit != "caddy.service" {
		t.Errorf("unit без суффикса: %q", s.Unit)
	}
	if c.Lines.Default != 100 || c.Lines.Max != 1000 || c.Docker.Access != DockerSudo {
		t.Errorf("умолчания: %+v %+v", c.Lines, c.Docker)
	}
	if c.Location.String() != "Asia/Novosibirsk" {
		t.Errorf("timezone: %v", c.Location)
	}
	for id, multi := range map[string]bool{"caddy": false, "immich": true, "kuma": false, "web": true, "mc": false, "nginx": true} {
		if s, _ := c.Source(id); s.IsMulti() != multi {
			t.Errorf("%s: IsMulti=%v", id, s.IsMulti())
		}
	}
	if c.ClampLines(0) != 100 || c.ClampLines(5000) != 1000 || c.ClampLines(7) != 7 {
		t.Error("ClampLines")
	}
}

func TestParseInvalid(t *testing.T) {
	cases := map[string]string{
		"опечатка в ключе":     "admins: [1]\nsourcs: []",
		"нет type":             "sources: [{id: a, unit: x}]",
		"два поля":             "sources: [{id: a, type: docker, container: x, match: y}]",
		"маска в unit":         "sources: [{id: a, type: systemd, unit: 'ca*'}]",
		"unit с дефиса":        "sources: [{id: a, type: systemd, unit: '--help'}]",
		"относительный путь":   "sources: [{id: a, type: file, path: logs/x.log}]",
		"путь с ..":            "sources: [{id: a, type: file, path: /srv/../etc/shadow}]",
		"контейнер с пробелом": "sources: [{id: a, type: docker, container: 'a b'}]",
		"пустой stack":         "sources: [{id: a, type: docker, match: 'stack:'}]",
		"повтор id":            "sources: [{id: a, type: systemd, unit: x}, {id: a, type: systemd, unit: y}]",
		"плохой id":            "sources: [{id: 'a b', type: systemd, unit: x}]",
		"плохой access":        "docker: {access: group}",
		"плохой timezone":      "timezone: Mars/Base",
		"плохая регулярка":     "redact: {extra: ['(']}",
		"max < default":        "lines: {default: 500, max: 100}",
	}
	for name, y := range cases {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("%s: ожидалась ошибка", name)
		}
	}
}

func TestCheckAdmins(t *testing.T) {
	for _, y := range []string{"admins: []", "admins: [0]", ""} {
		c, err := Parse([]byte(y))
		if err != nil {
			t.Fatal(err)
		}
		if c.CheckAdmins() == nil {
			t.Errorf("%q: ожидалась ошибка", y)
		}
	}
}

func TestLoadEnvFile(t *testing.T) {
	p := t.TempDir() + "/.env"
	if err := os.WriteFile(p, []byte("# comment\nLV_A=1\nexport LV_B=\"two words\"\nLV_C='x=y'\nbroken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LV_A", "keep")
	t.Setenv("LV_B", "")
	t.Setenv("LV_C", "")
	os.Unsetenv("LV_B")
	os.Unsetenv("LV_C")
	if err := LoadEnvFile(p); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"LV_A": "keep", "LV_B": "two words", "LV_C": "x=y"} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s=%q, want %q", k, got, want)
		}
	}
}
