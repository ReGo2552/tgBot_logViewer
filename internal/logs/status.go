package logs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ReGo2552/tgBot_logViewer/internal/config"
)

type ContainerStatus struct {
	Name      string `json:"name"`
	Project   string `json:"project,omitempty"`
	State     string `json:"state"`
	Health    string `json:"health"`
	Restarts  int    `json:"restarts"`
	ExitCode  int    `json:"exit_code"`
	OOMKilled bool   `json:"oom_killed"`
	CPU       string `json:"cpu"`
	Memory    string `json:"memory"`
}

// Status returns selected fields only; environment and healthcheck logs never leave Docker.
func (d *Docker) Status(ctx context.Context, cfg *config.Config) ([]ContainerStatus, error) {
	if d.Sudo {
		var out []ContainerStatus
		return out, d.helper(ctx, &out, "status")
	}
	cs, err := d.List(ctx)
	if err != nil {
		return nil, err
	}
	var names, running []string
	for _, c := range cs {
		if AllowedBy(cfg, c) && config.ValidContainerName(c.Name) {
			names = append(names, c.Name)
			if c.State == "running" {
				running = append(running, c.Name)
			}
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	format := `{"name":{{json .Name}},"state":{{json .State.Status}},"health":{{if .State.Health}}{{json .State.Health.Status}}{{else}}""{{end}},"restarts":{{.RestartCount}},"exit_code":{{.State.ExitCode}},"oom_killed":{{.State.OOMKilled}}}`
	o, err := runCmd(ctx, d.Cmd, d.Binary, append([]string{"inspect", "--format", format, "--"}, names...)...)
	if err != nil {
		return nil, err
	}
	var out []ContainerStatus
	for _, line := range bytes.Split(bytes.TrimSpace(o.stdout), []byte{'\n'}) {
		var c ContainerStatus
		if err := json.Unmarshal(line, &c); err != nil {
			return nil, fmt.Errorf("docker inspect: %w", err)
		}
		c.Name = strings.TrimPrefix(c.Name, "/")
		for _, listed := range cs {
			if listed.Name == c.Name {
				c.Project = listed.Project
				break
			}
		}
		out = append(out, c)
	}
	if len(running) == 0 {
		return out, nil
	}
	o, err = runCmd(ctx, d.Cmd, d.Binary, append([]string{"stats", "--no-stream", "--format", "{{json .}}", "--"}, running...)...)
	if err != nil {
		return out, fmt.Errorf("docker stats: %w", err)
	}
	for _, line := range bytes.Split(bytes.TrimSpace(o.stdout), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var st struct{ Name, CPUPerc, MemUsage string }
		if err := json.Unmarshal(line, &st); err != nil {
			return out, fmt.Errorf("docker stats: %w", err)
		}
		for i := range out {
			if out[i].Name == st.Name {
				out[i].CPU, out[i].Memory = st.CPUPerc, st.MemUsage
			}
		}
	}
	return out, nil
}
