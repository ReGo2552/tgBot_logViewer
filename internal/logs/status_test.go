package logs

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/ReGo2552/tgBot_logViewer/internal/config"
)

func TestStatusOnlyInspectsAllowedContainers(t *testing.T) {
	cfg, err := config.Parse([]byte(`docker: {access: direct}
sources: [{id: app, type: docker, match: "stack:app"}]`))
	if err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	d := &Docker{Binary: "docker", Cmd: func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls = append(calls, append([]string(nil), args...))
		var output string
		switch args[0] {
		case "ps":
			output = `{"Names":"web","State":"running","Labels":"com.docker.compose.project=app"}
{"Names":"secret","State":"running","Labels":"com.docker.compose.project=private"}`
		case "inspect":
			output = `{"name":"/web","state":"running","health":"healthy","restarts":3,"exit_code":0,"oom_killed":false}`
		case "stats":
			output = `{"Name":"web","CPUPerc":"12.5%","MemUsage":"10MiB / 1GiB"}`
		default:
			t.Errorf("unexpected command %s %v", name, args)
		}
		return exec.CommandContext(ctx, "printf", "%s", output)
	}}
	cs, err := d.Status(context.Background(), cfg)
	if err != nil || len(cs) != 1 || cs[0].Project != "app" || cs[0].CPU != "12.5%" || cs[0].Restarts != 3 {
		t.Fatalf("%+v %v", cs, err)
	}
	if len(calls) != 3 {
		t.Fatal(calls)
	}
	for _, args := range calls[1:] {
		if args[len(args)-1] != "web" || strings.Contains(strings.Join(args, " "), "secret") {
			t.Fatal("escaped allowlist", args)
		}
		for _, arg := range args {
			if arg == "{{json .}}" && args[0] == "inspect" {
				t.Fatal("full inspect leaks environment")
			}
		}
	}
}

func TestStatusSudoUsesFixedHelperOperation(t *testing.T) {
	d := &Docker{Sudo: true, Helper: "/usr/local/bin/logviewer", Cmd: func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name != "sudo" || strings.Join(args, " ") != "-n /usr/local/bin/logviewer docker-helper status" {
			t.Fatalf("%s %v", name, args)
		}
		return exec.CommandContext(ctx, "printf", "%s", `[{"name":"app","state":"running"}]`)
	}}
	cs, err := d.Status(context.Background(), &config.Config{})
	if err != nil || len(cs) != 1 {
		t.Fatalf("%+v %v", cs, err)
	}
}
