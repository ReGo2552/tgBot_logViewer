package install

import (
	"strings"
	"testing"
)

// С этими опциями systemd неявно включает NoNewPrivileges, и sudo перестаёт работать.
var impliesNoNewPrivs = []string{
	"NoNewPrivileges=true", "PrivateDevices=", "ProtectKernelTunables=", "ProtectKernelModules=",
	"ProtectKernelLogs=", "ProtectClock=", "RestrictSUIDSGID=", "RestrictNamespaces=",
	"LockPersonality=", "RestrictAddressFamilies=", "SystemCallFilter=", "SystemCallArchitectures=",
	"MemoryDenyWriteExecute=", "RestrictRealtime=", "DynamicUser=",
}

func TestUnitSudo(t *testing.T) {
	u := Unit("logbot", true)
	for _, opt := range impliesNoNewPrivs {
		if strings.Contains(u, "\n"+opt) {
			t.Errorf("в sudo-режиме нельзя %s", opt)
		}
	}
	for _, must := range []string{"User=logbot", "NoNewPrivileges=false", "ProtectHome=read-only", "EnvironmentFile=/etc/log-viewer/.env"} {
		if !strings.Contains(u, must) {
			t.Errorf("нет %s", must)
		}
	}
	if !strings.Contains(Unit("logbot", false), "NoNewPrivileges=true") {
		t.Error("без sudo изоляция должна быть строже")
	}
}

func TestSudoers(t *testing.T) {
	s := Sudoers("logbot")
	if !strings.Contains(s, "logbot ALL=(root) NOPASSWD: /usr/local/bin/logviewer docker-helper *\n") {
		t.Errorf("sudoers:\n%s", s)
	}
}
