package monitor

import (
	"context"
	"os/exec"
	"testing"

	"github.com/ReGo2552/tgBot_logViewer/internal/config"
)

func TestServiceFailureVersusMissingUnit(t *testing.T) {
	cfg, err := config.Parse([]byte("sources: [{id: app, type: systemd, unit: app}]"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body string
		exit       bool
		missing    bool
	}{
		{"running", "LoadState=loaded\nActiveState=active\nSubState=running\nMainPID=42\nNRestarts=3\nExecMainStatus=0\nResult=success\n", false, false},
		{"missing", "LoadState=not-found\nActiveState=inactive\n", true, true},
		{"unavailable", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mk := func(ctx context.Context, name string, args ...string) *exec.Cmd {
				if name != "systemctl" || args[len(args)-2] != "--" || args[len(args)-1] != "app.service" {
					t.Fatal(name, args)
				}
				if tc.exit {
					return exec.CommandContext(ctx, "sh", "-c", `printf '%s' "$1"; exit 1`, "test", tc.body)
				}
				return exec.CommandContext(ctx, "printf", "%s", tc.body)
			}
			services := Services(context.Background(), cfg, mk)
			if len(services) != 1 || services[0].Missing != tc.missing {
				t.Fatal(services)
			}
			if !tc.exit && (services[0].State != "active" || services[0].Restarts != "3") {
				t.Fatal(services)
			}
			if tc.exit && services[0].Error == "" {
				t.Fatal("command failure lost")
			}
		})
	}
}
