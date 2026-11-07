package monitor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ReGo2552/tgBot_logViewer/internal/config"
	"github.com/ReGo2552/tgBot_logViewer/internal/logs"
)

type Service struct {
	SourceID, Name, State, SubState, Result string
	PID, Restarts, ExitCode                 string
	Memory                                  string
	Error                                   string
	Missing                                 bool
}

func Services(ctx context.Context, cfg *config.Config, mk logs.CommandFunc) []Service {
	var out []Service
	for _, src := range cfg.Sources {
		if src.Type != config.TypeSystemd {
			continue
		}
		s := Service{SourceID: src.ID, Name: src.Label()}
		tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		b, err := mk(tctx, "systemctl", "show", "--no-pager", "--property=LoadState,ActiveState,SubState,Result,MainPID,NRestarts,ExecMainStatus,MemoryCurrent", "--", src.Unit).Output()
		cancel()
		p := map[string]string{}
		for _, line := range strings.Split(string(b), "\n") {
			k, v, ok := strings.Cut(line, "=")
			if ok {
				p[k] = v
			}
		}
		s.Missing = p["LoadState"] == "not-found"
		if err != nil && !s.Missing {
			s.Error = fmt.Sprintf("systemctl: %v", err)
			out = append(out, s)
			continue
		}
		s.State, s.SubState, s.Result = p["ActiveState"], p["SubState"], p["Result"]
		s.PID, s.Restarts, s.ExitCode, s.Memory = p["MainPID"], p["NRestarts"], p["ExecMainStatus"], p["MemoryCurrent"]
		if p["LoadState"] != "loaded" {
			s.Error = "unit: " + p["LoadState"]
		}
		out = append(out, s)
	}
	return out
}
