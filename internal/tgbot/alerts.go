package tgbot

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ReGo2552/tgBot_logViewer/internal/alerts"
	"github.com/ReGo2552/tgBot_logViewer/internal/config"
	"github.com/ReGo2552/tgBot_logViewer/internal/logs"
	"github.com/ReGo2552/tgBot_logViewer/internal/monitor"
	"github.com/ReGo2552/tgBot_logViewer/internal/tgapi"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

func observations(cfg *config.Config, s monitor.Snapshot, r monitor.Resources, now time.Time) []alerts.Observation {
	var out []alerts.Observation
	a := cfg.Alerts
	metric := func(key, title string, value, threshold float64) {
		out = append(out, alerts.Observation{Key: key, Title: fmt.Sprintf("%s: %.1f%%", title, value), Bad: value >= threshold, Clear: value <= threshold-a.RecoveryGap})
	}
	if !s.Time.IsZero() && now.Sub(s.Time) <= 2*cfg.Monitor.Interval+time.Second {
		if s.CPUReady {
			metric("cpu", "CPU", s.CPU, a.CPU)
		}
		if s.MemoryReady {
			metric("ram", "RAM", s.RAMPercent(), a.RAM)
		}
		for _, d := range s.Disks {
			metric("disk:"+d.Path, "Диск "+d.Path, d.Percent, a.Disk)
			metric("inode:"+d.Path, "Inode "+d.Path, d.Inodes, a.Inodes)
		}
	}
	if r.Time.IsZero() || now.Sub(r.Time) > time.Minute {
		return out
	}
	for _, id := range a.WatchSources {
		src, ok := cfg.Source(id)
		if !ok {
			continue
		}
		o := alerts.Observation{Key: "source:" + id, Title: src.Label(), SourceID: id}
		switch src.Type {
		case config.TypeSystemd:
			found := false
			for _, svc := range r.Services {
				if svc.SourceID != id || (svc.Error != "" && !svc.Missing) {
					continue
				}
				found = true
				o.Title += " · " + svc.State + "/" + svc.SubState + " · exit " + svc.ExitCode
				if svc.Missing {
					o.Title = src.Label() + " · служба не найдена"
				}
				o.Bad = svc.Missing || svc.State == "failed" || svc.State == "inactive" || svc.State == "deactivating"
				o.Clear = !svc.Missing && svc.State == "active"
			}
			if !found {
				continue
			}
		case config.TypeDocker:
			if r.DockerError != "" {
				continue
			}
			count := 0
			o.Clear = true
			for _, c := range r.Containers {
				if !logs.Matches(src, logs.Container{Name: c.Name, Project: c.Project}) {
					continue
				}
				count++
				if c.State != "running" || c.Health == "unhealthy" {
					o.Bad = true
					o.Clear = false
					o.Title += " · " + c.Name + ": " + c.State + " " + c.Health
				} else if c.Health == "starting" {
					o.Clear = false
				}
			}
			if count == 0 {
				o.Bad = true
				o.Clear = false
				o.Title += " · контейнеры не найдены"
			}
			if o.Clear {
				o.Title += " · работает"
			}
		}
		o.Title = shortText(o.Title, 400)
		out = append(out, o)
	}
	return out
}

func (a *App) runAlerts(ctx context.Context, b *bot.Bot) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	var next time.Time
	retry := map[int64]time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if !now.Before(next) {
				if err := a.alerts.Evaluate(now, observations(a.Config, a.monitor.Snapshot(), a.resources.Snapshot(), now)); err != nil {
					a.Log.Error("сохранение уведомлений", "err", err)
				}
				next = now.Add(a.Config.Alerts.Interval)
			}
			// One delivery per chat per pass. Failed sends stay in the durable outbox.
			for _, d := range a.alerts.Pending(now) {
				if retry[d.ChatID].After(now) {
					continue
				}
				retry[d.ChatID] = now.Add(2 * time.Second)
				icon, prefix := "🔴", "Проблема"
				if d.Event.Recovered {
					icon, prefix = "🟢", "Восстановилось"
				}
				text := fmt.Sprintf("%s <b>%s</b>\n%s\n%s", icon, prefix, esc(shortText(d.Event.Title, 500)), d.Event.Time.In(a.Config.Location).Format("01-02 15:04:05"))
				row := []models.InlineKeyboardButton{panelButton("Ресурсы", "status"), panelButton("🔕 30 минут", "alerts:mute")}
				if d.Event.SourceID != "" {
					row = append(row, models.InlineKeyboardButton{Text: "📄 Логи", CallbackData: "s:" + d.Event.SourceID})
				}
				tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				_, err := b.SendMessage(tctx, &bot.SendMessageParams{ChatID: d.ChatID, Text: text, ParseMode: models.ParseModeHTML, ReplyMarkup: &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{row}}})
				cancel()
				if err != nil {
					a.Log.Warn("уведомление не доставлено", "chat", d.ChatID, "err", tgapi.Scrub(err, a.Env.Token))
					retry[d.ChatID] = now.Add(time.Minute)
					var limited *bot.TooManyRequestsError
					if errors.As(err, &limited) {
						retry[d.ChatID] = now.Add(max(time.Minute, time.Duration(limited.RetryAfter)*time.Second))
					}
					continue
				}
				if err := a.alerts.Ack(d.ID); err != nil {
					a.Log.Error("подтверждение уведомления", "err", err)
				}
			}
		}
	}
}
