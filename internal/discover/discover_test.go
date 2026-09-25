package discover

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ReGo2552/tgBot_logViewer/internal/access"
	"github.com/ReGo2552/tgBot_logViewer/internal/config"
	"github.com/ReGo2552/tgBot_logViewer/internal/logs"
)

// Вывод systemctl show с home-server (сокращён).
const showOut = `Id=mc_bot.service
Description=Minecraft Telegram control bot
FragmentPath=/etc/systemd/system/mc_bot.service
LoadState=loaded
ActiveState=active
UnitFileState=enabled
WorkingDirectory=/home/erik/bots/mc_bot
StandardOutput=journal
ExecStart={ path=/home/erik/bots/mc_bot/.venv/bin/python ; argv[]=/home/erik/bots/mc_bot/.venv/bin/python /home/erik/bots/mc_bot/main.py ; ignore_errors=no }

Id=kino-monitor.service
Description=megakino42 schedule monitor
FragmentPath=/etc/systemd/system/kino-monitor.service
LoadState=loaded
ActiveState=inactive
UnitFileState=disabled
WorkingDirectory=/home/erik/bots/kino-monitor
StandardOutput=journal
ExecStart={ path=/usr/bin/python3 ; argv[]=/usr/bin/python3 /home/erik/bots/kino-monitor/monitor.py }

Id=caddy.service
Description=Caddy
FragmentPath=/usr/lib/systemd/system/caddy.service
LoadState=loaded
ActiveState=active
UnitFileState=enabled
ExecStart={ path=/usr/bin/caddy ; argv[]=/usr/bin/caddy run }

Id=colord.service
Description=Manage, Install and Generate Color Profiles
FragmentPath=/usr/lib/systemd/system/colord.service
LoadState=loaded
ActiveState=active
SubState=running
UnitFileState=static

Id=apparmor.service
FragmentPath=/usr/lib/systemd/system/apparmor.service
LoadState=loaded
ActiveState=active
SubState=exited
UnitFileState=enabled

Id=user@1000.service
FragmentPath=/usr/lib/systemd/system/user@.service
LoadState=loaded
ActiveState=active
SubState=running

Id=apt-daily.service
FragmentPath=/usr/lib/systemd/system/apt-daily.service
LoadState=loaded
ActiveState=inactive
UnitFileState=static

Id=snap.v2raya.v2raya.service
FragmentPath=/etc/systemd/system/snap.v2raya.v2raya.service
LoadState=loaded
ActiveState=inactive
UnitFileState=disabled

Id=ollama.service
FragmentPath=/etc/systemd/system/ollama.service
LoadState=loaded
ActiveState=active
UnitFileState=enabled
ExecStart={ path=/usr/local/bin/ollama ; argv[]=/usr/local/bin/ollama serve }

Id=mybot.service
FragmentPath=/usr/lib/systemd/system/mybot.service
LoadState=loaded
ActiveState=failed
StandardOutput=append:/var/log/mybot.log
ExecStart={ path=/opt/mybot/bin/mybot ; argv[]=/opt/mybot/bin/mybot }
`

func TestClassifyUnits(t *testing.T) {
	units := parseShow([]byte(showOut))
	if len(units) != 10 {
		t.Fatalf("юнитов %d", len(units))
	}
	got := map[string]Candidate{}
	for _, u := range units {
		if c, ok := classifyUnit(u); ok {
			got[u.ID] = c
		}
	}
	want := map[string]bool{ // id → Enabled; отсутствие — отброшен
		"mc_bot.service": true, "kino-monitor.service": true, "caddy.service": true,
		"colord.service": false, "ollama.service": true, "mybot.service": true,
	}
	for id, en := range want {
		c, ok := got[id]
		if !ok {
			t.Errorf("%s отброшен", id)
			continue
		}
		if c.Enabled != en {
			t.Errorf("%s: Enabled=%v (%s)", id, c.Enabled, c.Note)
		}
	}
	for _, id := range []string{"apt-daily.service", "snap.v2raya.v2raya.service", "apparmor.service", "user@1000.service"} {
		if _, ok := got[id]; ok {
			t.Errorf("%s не должен предлагаться", id)
		}
	}
	for _, u := range units {
		if u.ID == "mybot.service" && u.stdoutFile() != "/var/log/mybot.log" {
			t.Errorf("stdout в файл: %q", u.stdoutFile())
		}
		if u.ID == "mc_bot.service" && appDir(u.ExecPath) != "/home/erik/bots/mc_bot" {
			t.Errorf("appDir: %q", appDir(u.ExecPath))
		}
	}
}

func homeContainers() []logs.Container {
	return []logs.Container{
		{Name: "collabora", State: "running", Project: "collabora"},
		{Name: "immich_machine_learning", State: "running", Project: "immich"},
		{Name: "immich_postgres", State: "running", Project: "immich"},
		{Name: "immich_server", State: "running", Project: "immich"},
		{Name: "nextcloud_app", State: "running", Project: "nextcloud"},
		{Name: "nextcloud_db", State: "running", Project: "nextcloud"},
		{Name: "open-webui", State: "running", Project: "llm-webui"},
		{Name: "portainer", State: "running"},
		{Name: "old", State: "exited"},
	}
}

func TestDockerCandidates(t *testing.T) {
	cs := dockerCandidates(homeContainers())
	var got []string
	for _, c := range cs {
		s := c.Source
		got = append(got, s.ID+"="+s.Container+s.Match)
	}
	want := "collabora=collabora|immich=stack:immich|nextcloud=stack:nextcloud|old=old|open-webui=open-webui|portainer=portainer"
	if strings.Join(got, "|") != want {
		t.Errorf("\n got %s\nwant %s", strings.Join(got, "|"), want)
	}
}

func sampleResult() *Result {
	r := &Result{Containers: homeContainers()}
	for _, u := range parseShow([]byte(showOut)) {
		if c, ok := classifyUnit(u); ok {
			r.Systemd = append(r.Systemd, c)
		}
	}
	r.Docker = dockerCandidates(r.Containers)
	r.Files = []Candidate{
		{Source: config.Source{Type: config.TypeFile, Path: "/srv/minecraft/logs/latest.log"}, Enabled: true,
			Problem: &access.Problem{What: "logbot не может войти в каталог /srv/minecraft", Fix: "sudo setfacl -m u:logbot:x /srv/minecraft"}},
		{Source: config.Source{Type: config.TypeFile, Path: "/srv/minecraft/plugins/AuthMe/authme.log"}},
		{Source: config.Source{Type: config.TypeFile, Path: "/srv/weird dir/on.log"}},
	}
	r.AssignIDs(nil)
	return r
}

// Черновик обязан парситься — иначе install положит нерабочий конфиг.
func TestRenderConfigRoundTrip(t *testing.T) {
	r := sampleResult()
	text := RenderConfig(r, Meta{Host: "home-server", Admins: []int64{42}, User: "logbot", Generated: time.Now()})
	cfg, err := config.Parse([]byte(text))
	if err != nil {
		t.Fatalf("%v\n%s", err, text)
	}
	ids := map[string]bool{}
	for _, s := range cfg.Sources {
		ids[s.ID] = true
	}
	for _, id := range []string{"mc_bot", "caddy", "immich", "minecraft-latest", "portainer"} {
		if !ids[id] {
			t.Errorf("нет включённого %s", id)
		}
	}
	for _, id := range []string{"colord", "authme", "weird-dir-on"} {
		if ids[id] {
			t.Errorf("%s должен быть закомментирован", id)
		}
		if !strings.Contains(text, "id: "+id) {
			t.Errorf("кандидата %s нет даже в комментариях", id)
		}
	}
	if !strings.Contains(text, "исправить: sudo setfacl -m u:logbot:x /srv/minecraft") {
		t.Error("нет подсказки по правам")
	}
	if !strings.Contains(text, `"/srv/weird dir/on.log"`) {
		t.Error("путь с пробелом должен быть в кавычках")
	}

	// без admins тоже парсится (admins проверяются отдельно)
	if _, err := config.Parse([]byte(RenderConfig(r, Meta{}))); err != nil {
		t.Error(err)
	}
	if _, err := config.Parse([]byte(RenderConfig(&Result{}, Meta{}))); err != nil {
		t.Errorf("пустой: %v", err)
	}
}

func TestMissing(t *testing.T) {
	cfg, err := config.Parse([]byte(`
sources:
  - {id: mc_bot, type: systemd, unit: mc_bot.service}
  - {id: immich, type: docker, match: "stack:immich"}
  - {id: nc, type: docker, container: nextcloud_app}
  - {id: mc, type: file, path: "/srv/minecraft/logs/*.log"}
`))
	if err != nil {
		t.Fatal(err)
	}
	m := Missing(cfg, sampleResult())
	var got []string
	for _, c := range m.All() {
		got = append(got, c.Source.ID)
	}
	// nextcloud: nextcloud_db не покрыт — стек предлагается целиком
	want := "kino-monitor|caddy|ollama|mybot|collabora|nextcloud|old|open-webui|portainer"
	if strings.Join(got, "|") != want {
		t.Errorf("\n got %s\nwant %s", strings.Join(got, "|"), want)
	}
}

func TestIDs(t *testing.T) {
	cases := map[string]string{
		"/srv/minecraft/logs/latest.log":           "minecraft-latest",
		"/srv/minecraft/logs/backup.log":           "minecraft-backup",
		"/srv/minecraft/plugins/AuthMe/authme.log": "authme",
		"/home/erik/bots/mybot/bot.log":            "mybot-bot",
		"/var/log/mybot.log":                       "mybot",
	}
	for p, want := range cases {
		if got := sanitizeID(baseID(config.Source{Type: config.TypeFile, Path: p})); got != want {
			t.Errorf("%s → %s, want %s", p, got, want)
		}
	}
	r := &Result{Systemd: []Candidate{
		{Source: config.Source{Type: config.TypeSystemd, Unit: "caddy.service"}},
		{Source: config.Source{Type: config.TypeSystemd, Unit: "Caddy.service"}},
	}}
	r.AssignIDs(map[string]bool{"caddy": true})
	if r.Systemd[0].Source.ID != "caddy-2" || r.Systemd[1].Source.ID != "caddy-3" {
		t.Errorf("уникальность: %s %s", r.Systemd[0].Source.ID, r.Systemd[1].Source.ID)
	}
}

func TestScanWorkdirs(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"bot.log", "logs/app.log", "data/deep/x.log", ".venv/lib/y.log", "notes.txt"} {
		full := filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte("x"), 0o644)
	}
	got := map[string]bool{}
	for _, c := range scanWorkdirs([]workdir{{dir: root, unit: "x.service"}}) {
		got[strings.TrimPrefix(c.Source.Path, root+"/")] = c.Enabled
	}
	want := map[string]bool{"bot.log": true, "logs/app.log": true, "data/deep/x.log": false}
	if len(got) != len(want) {
		t.Errorf("найдено %v", got)
	}
	for p, en := range want {
		if e, ok := got[p]; !ok || e != en {
			t.Errorf("%s: %v %v", p, ok, e)
		}
	}
}
