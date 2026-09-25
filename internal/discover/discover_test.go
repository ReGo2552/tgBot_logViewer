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

// Вывод systemctl show (сокращён; юниты вымышленные, но типичные).
const showOut = `Id=weather_bot.service
Description=Weather Telegram bot
FragmentPath=/etc/systemd/system/weather_bot.service
LoadState=loaded
ActiveState=active
UnitFileState=enabled
WorkingDirectory=/home/alice/bots/weather_bot
StandardOutput=journal
ExecStart={ path=/home/alice/bots/weather_bot/.venv/bin/python ; argv[]=/home/alice/bots/weather_bot/.venv/bin/python /home/alice/bots/weather_bot/main.py ; ignore_errors=no }

Id=backup-report.service
Description=Nightly backup report
FragmentPath=/etc/systemd/system/backup-report.service
LoadState=loaded
ActiveState=inactive
UnitFileState=disabled
WorkingDirectory=/home/alice/scripts/backup-report
StandardOutput=journal
ExecStart={ path=/usr/bin/python3 ; argv[]=/usr/bin/python3 /home/alice/scripts/backup-report/report.py }

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

Id=snap.lxd.daemon.service
FragmentPath=/etc/systemd/system/snap.lxd.daemon.service
LoadState=loaded
ActiveState=inactive
UnitFileState=disabled

Id=node_exporter.service
FragmentPath=/usr/lib/systemd/system/node_exporter.service
LoadState=loaded
ActiveState=active
UnitFileState=enabled
ExecStart={ path=/usr/local/bin/node_exporter ; argv[]=/usr/local/bin/node_exporter }

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
		"weather_bot.service": true, "backup-report.service": true, "caddy.service": true,
		"colord.service": false, "node_exporter.service": true, "mybot.service": true,
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
	for _, id := range []string{"apt-daily.service", "snap.lxd.daemon.service", "apparmor.service", "user@1000.service"} {
		if _, ok := got[id]; ok {
			t.Errorf("%s не должен предлагаться", id)
		}
	}
	for _, u := range units {
		if u.ID == "mybot.service" && u.stdoutFile() != "/var/log/mybot.log" {
			t.Errorf("stdout в файл: %q", u.stdoutFile())
		}
		if u.ID == "weather_bot.service" && appDir(u.ExecPath) != "/home/alice/bots/weather_bot" {
			t.Errorf("appDir: %q", appDir(u.ExecPath))
		}
	}
}

func sampleContainers() []logs.Container {
	return []logs.Container{
		{Name: "vaultwarden", State: "running", Project: "vaultwarden"},
		{Name: "paperless_broker", State: "running", Project: "paperless"},
		{Name: "paperless_db", State: "running", Project: "paperless"},
		{Name: "paperless_web", State: "running", Project: "paperless"},
		{Name: "gitea_web", State: "running", Project: "gitea"},
		{Name: "gitea_db", State: "running", Project: "gitea"},
		{Name: "jellyfin", State: "running", Project: "media"},
		{Name: "watchtower", State: "running"},
		{Name: "old", State: "exited"},
	}
}

func TestDockerCandidates(t *testing.T) {
	cs := dockerCandidates(sampleContainers())
	var got []string
	for _, c := range cs {
		s := c.Source
		got = append(got, s.ID+"="+s.Container+s.Match)
	}
	want := "gitea=stack:gitea|jellyfin=jellyfin|old=old|paperless=stack:paperless|vaultwarden=vaultwarden|watchtower=watchtower"
	if strings.Join(got, "|") != want {
		t.Errorf("\n got %s\nwant %s", strings.Join(got, "|"), want)
	}
}

func sampleResult() *Result {
	r := &Result{Containers: sampleContainers()}
	for _, u := range parseShow([]byte(showOut)) {
		if c, ok := classifyUnit(u); ok {
			r.Systemd = append(r.Systemd, c)
		}
	}
	r.Docker = dockerCandidates(r.Containers)
	r.Files = []Candidate{
		{Source: config.Source{Type: config.TypeFile, Path: "/srv/factorio/logs/current.log"}, Enabled: true,
			Problem: &access.Problem{What: "logbot не может войти в каталог /srv/factorio", Fix: "sudo setfacl -m u:logbot:x /srv/factorio"}},
		{Source: config.Source{Type: config.TypeFile, Path: "/srv/factorio/mods/chatlog/chatlog.log"}},
		{Source: config.Source{Type: config.TypeFile, Path: "/srv/weird dir/on.log"}},
	}
	r.AssignIDs(nil)
	return r
}

// Черновик обязан парситься — иначе install положит нерабочий конфиг.
func TestRenderConfigRoundTrip(t *testing.T) {
	r := sampleResult()
	text := RenderConfig(r, Meta{Host: "example", Admins: []int64{42}, User: "logbot", Generated: time.Now()})
	cfg, err := config.Parse([]byte(text))
	if err != nil {
		t.Fatalf("%v\n%s", err, text)
	}
	ids := map[string]bool{}
	for _, s := range cfg.Sources {
		ids[s.ID] = true
	}
	for _, id := range []string{"weather_bot", "caddy", "paperless", "factorio-current", "watchtower"} {
		if !ids[id] {
			t.Errorf("нет включённого %s", id)
		}
	}
	for _, id := range []string{"colord", "chatlog", "weird-dir-on"} {
		if ids[id] {
			t.Errorf("%s должен быть закомментирован", id)
		}
		if !strings.Contains(text, "id: "+id) {
			t.Errorf("кандидата %s нет даже в комментариях", id)
		}
	}
	if !strings.Contains(text, "исправить: sudo setfacl -m u:logbot:x /srv/factorio") {
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
  - {id: weather_bot, type: systemd, unit: weather_bot.service}
  - {id: paperless, type: docker, match: "stack:paperless"}
  - {id: git, type: docker, container: gitea_web}
  - {id: factorio, type: file, path: "/srv/factorio/logs/*.log"}
`))
	if err != nil {
		t.Fatal(err)
	}
	m := Missing(cfg, sampleResult())
	var got []string
	for _, c := range m.All() {
		got = append(got, c.Source.ID)
	}
	// gitea: gitea_db не покрыт — стек предлагается целиком
	want := "backup-report|caddy|node_exporter|mybot|gitea|jellyfin|old|vaultwarden|watchtower"
	if strings.Join(got, "|") != want {
		t.Errorf("\n got %s\nwant %s", strings.Join(got, "|"), want)
	}
}

func TestIDs(t *testing.T) {
	cases := map[string]string{
		"/srv/factorio/logs/current.log":         "factorio-current",
		"/srv/factorio/logs/backup.log":          "factorio-backup",
		"/srv/factorio/mods/chatlog/chatlog.log": "chatlog",
		"/home/alice/bots/mybot/bot.log":         "mybot-bot",
		"/var/log/mybot.log":                     "mybot",
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
