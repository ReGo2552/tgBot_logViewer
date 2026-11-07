package monitor

import (
	"context"
	"sync"
	"time"

	"github.com/ReGo2552/tgBot_logViewer/internal/config"
	"github.com/ReGo2552/tgBot_logViewer/internal/logs"
)

type Resources struct {
	Time        time.Time
	Services    []Service
	Containers  []logs.ContainerStatus
	DockerError string
}

type ResourceCache struct {
	mu   sync.RWMutex
	data Resources
}

func (c *ResourceCache) Snapshot() Resources {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := c.data
	s.Services = append([]Service(nil), s.Services...)
	s.Containers = append([]logs.ContainerStatus(nil), s.Containers...)
	return s
}

func (c *ResourceCache) Run(ctx context.Context, cfg *config.Config, d *logs.Docker) {
	refresh := func() {
		r := Resources{}
		// Independent deadlines: one failing backend must not starve the other.
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			tctx, cancel := context.WithTimeout(ctx, 25*time.Second)
			defer cancel()
			r.Services = Services(tctx, cfg, logs.DefaultCommand)
		}()
		go func() {
			defer wg.Done()
			if !cfg.HasType(config.TypeDocker) {
				return
			}
			tctx, cancel := context.WithTimeout(ctx, 25*time.Second)
			defer cancel()
			var err error
			r.Containers, err = d.Status(tctx, cfg)
			if err != nil {
				r.DockerError = err.Error()
			}
		}()
		wg.Wait()
		r.Time = time.Now()
		c.mu.Lock()
		c.data = r
		c.mu.Unlock()
	}
	refresh()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			refresh()
		}
	}
}
