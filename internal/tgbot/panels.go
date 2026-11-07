package tgbot

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/ReGo2552/tgBot_logViewer/internal/logs"
	"github.com/ReGo2552/tgBot_logViewer/internal/monitor"
	"github.com/ReGo2552/tgBot_logViewer/internal/redact"
)

func panelButton(text, arg string) models.InlineKeyboardButton {
	return models.InlineKeyboardButton{Text: text, CallbackData: "p:" + arg}
}

func (a *App) panel(ctx context.Context, b *bot.Bot, chatID int64, msg *models.Message, arg string) {
	if a.monitor == nil {
		a.send(ctx, b, chatID, "Мониторинг отключён: monitor.disable в конфиге.", nil)
		return
	}
	var text string
	var rows [][]models.InlineKeyboardButton
	s := a.monitor.Snapshot()
	r := a.resources.Snapshot()
	kind, opt, _ := strings.Cut(arg, ":")
	switch kind {
	case "status":
		text = renderStatus(s, a.Config.Location)
	case "top":
		text = renderTop(s, opt, a.Config.Monitor.TopLimit, a.Config.Location)
		rows = append(rows, []models.InlineKeyboardButton{panelButton("По CPU", "top:cpu"), panelButton("По памяти", "top:mem")})
	case "services":
		text, rows = renderServices(r, opt, a.Config.Location)
	case "containers":
		text, rows = a.renderContainers(r, opt, a.Config.Location)
	case "alerts":
		if a.alerts == nil {
			text = "Уведомления отключены. Включите alerts.enabled в конфиге."
			break
		}
		if opt == "mute" || opt == "unmute" {
			until := time.Time{}
			if opt == "mute" {
				until = time.Now().Add(30 * time.Minute)
			}
			if err := a.alerts.Mute(chatID, until); err != nil {
				a.Log.Error("сохранение mute", "err", err)
				a.send(ctx, b, chatID, "Не удалось сохранить настройку уведомлений.", nil)
				return
			}
			arg = "alerts"
		}
		active, history, muted := a.alerts.View(chatID)
		text = "<b>Уведомления</b>\n"
		if muted.After(time.Now()) {
			text += "🔕 До " + muted.In(a.Config.Location).Format("15:04") + "\n"
		}
		text += fmt.Sprintf("Активных проблем: %d\n", len(active))
		for i, s := range active {
			if i == 10 {
				text += "…\n"
				break
			}
			text += "🔴 " + esc(shortText(s.Title, 150)) + " · с " + s.Since.In(a.Config.Location).Format("01-02 15:04") + "\n"
			if time.Since(s.LastSeen) > 2*a.Config.Alerts.Interval {
				text += "⚠ Нет свежих данных для этой проблемы\n"
			}
		}
		text += "\nПоследние события:\n"
		for i := len(history) - 1; i >= max(0, len(history)-8); i-- {
			ev := history[i]
			icon := "🔴"
			if ev.Recovered {
				icon = "🟢"
			}
			text += icon + " " + ev.Time.In(a.Config.Location).Format("01-02 15:04") + " " + esc(shortText(ev.Title, 150)) + "\n"
		}
		if len(history) == 0 {
			text += "Пока нет событий.\n"
		}
		text += "\nПравило срабатывает после " + a.Config.Alerts.Hold.String() + " непрерывного превышения."
		if muted.After(time.Now()) {
			rows = append(rows, []models.InlineKeyboardButton{panelButton("🔔 Включить", "alerts:unmute")})
		} else {
			rows = append(rows, []models.InlineKeyboardButton{panelButton("🔕 На 30 минут", "alerts:mute")})
		}
	default:
		return
	}
	rows = append(rows, []models.InlineKeyboardButton{panelButton("🔄 Обновить", arg)})
	rows = append(rows, []models.InlineKeyboardButton{panelButton("Ресурсы", "status"), panelButton("Процессы", "top")}, []models.InlineKeyboardButton{panelButton("Службы", "services"), panelButton("Контейнеры", "containers"), panelButton("Проблемы", "alerts")})
	a.sendOrEdit(ctx, b, chatID, msg, text, &models.InlineKeyboardMarkup{InlineKeyboard: rows})
}

func renderStatus(s monitor.Snapshot, loc *time.Location) string {
	if s.Time.IsZero() {
		return "Собираю первый снимок ресурсов. Нажмите «Обновить»."
	}
	cpu := "первый замер…"
	if s.CPUReady {
		cpu = fmt.Sprintf("%.1f%% (%d CPU)", s.CPU, s.CPUs)
	}
	mem := "недоступна"
	if s.MemoryReady {
		mem = fmt.Sprintf("%s / %s · %.1f%%", size(s.MemoryUsed), size(s.MemoryTotal), s.RAMPercent())
	}
	net := "первый замер…"
	if s.NetworkReady {
		net = fmt.Sprintf("↓ %s/с · ↑ %s/с", size(uint64(s.RX)), size(uint64(s.TX)))
	}
	text := fmt.Sprintf("<b>%s</b> · %s\nUptime: %s\n\n<b>CPU:</b> %s\n<b>Load 1/5/15m:</b> %s\n<b>RAM:</b> %s\n<b>Swap:</b> %s / %s\n<b>Сеть:</b> %s\n", esc(shortText(s.Host, 100)), s.Time.In(loc).Format("15:04:05"), s.Uptime.Truncate(time.Minute), cpu, esc(s.Load), mem, size(s.SwapUsed), size(s.SwapTotal), net)
	if len(s.History) > 1 {
		text += "CPU: <code>" + sparkline(s.History) + "</code>\n"
	}
	for i, d := range s.Disks {
		if i == 10 {
			text += "… остальные диски скрыты\n"
			break
		}
		text += fmt.Sprintf("\n<b>%s:</b> %s / %s · %.1f%%\nInode: %.1f%%\n", esc(shortText(d.Path, 150)), size(d.Used), size(d.Total), d.Percent, d.Inodes)
	}
	text += fmt.Sprintf("\nВидимых процессов: %d", len(s.Processes))
	if time.Since(s.Time) > time.Minute {
		text += "\n⚠ Данные устарели"
	}
	for i, w := range s.Warnings {
		if i == 3 {
			break
		}
		text += "\n⚠ " + esc(shortText(w, 180))
	}
	return text
}

func renderTop(s monitor.Snapshot, order string, limit int, loc *time.Location) string {
	if s.Time.IsZero() {
		return "Собираю процессы. Нажмите «Обновить»."
	}
	ps := s.Processes
	// Snapshot already owns its slice; copy here as well for callers/tests reusing it.
	ps = append([]monitor.Process(nil), ps...)
	sort.SliceStable(ps, func(i, j int) bool {
		if order == "mem" {
			return ps[i].RSS > ps[j].RSS
		}
		return ps[i].CPU > ps[j].CPU
	})
	var out strings.Builder
	out.WriteString("<b>Процессы</b> · " + s.Time.In(loc).Format("15:04:05") + "\nCPU: 100% = одно ядро. Аргументы запуска скрыты.\n")
	if !s.CPUReady {
		out.WriteString("CPU появится после второго замера.\n")
	}
	out.WriteString("<pre>PID      CPU     RAM USER       NAME\n")
	for _, p := range ps[:min(max(limit, 1), len(ps))] {
		fmt.Fprintf(&out, "%-7d %5.1f%% %7s %-10s %s\n", p.PID, p.CPU, size(p.RSS), esc(shortText(p.User, 10)), esc(shortText(p.Name, 24)))
	}
	out.WriteString("</pre>")
	return out.String()
}

func pageIndex(raw string, count int) (int, int, int) {
	p, _ := strconv.Atoi(raw)
	p = max(0, min(p, max(0, (count-1)/8)))
	return p, p * 8, min((p+1)*8, count)
}

func pageButtons(kind string, p, count int) [][]models.InlineKeyboardButton {
	var row []models.InlineKeyboardButton
	if p > 0 {
		row = append(row, panelButton("←", fmt.Sprintf("%s:%d", kind, p-1)))
	}
	if (p+1)*8 < count {
		row = append(row, panelButton("→", fmt.Sprintf("%s:%d", kind, p+1)))
	}
	if len(row) > 0 {
		return [][]models.InlineKeyboardButton{row}
	}
	return nil
}

func renderServices(r monitor.Resources, opt string, loc *time.Location) (string, [][]models.InlineKeyboardButton) {
	if r.Time.IsZero() {
		return "Собираю состояние служб. Нажмите «Обновить».", nil
	}
	text := "<b>Службы из конфига</b>\n"
	p, start, end := pageIndex(opt, len(r.Services))
	var rows [][]models.InlineKeyboardButton
	for _, s := range r.Services[start:end] {
		icon := "⚪"
		if s.State == "active" {
			icon = "🟢"
		}
		if s.State == "failed" {
			icon = "🔴"
		}
		if s.Error != "" {
			text += "⚠ " + esc(shortText(s.Name, 80)) + ": " + esc(shortText(s.Error, 180)) + "\n"
		} else {
			text += fmt.Sprintf("%s <b>%s</b> · %s/%s\nPID %s · рестартов %s · exit %s · %s\n", icon, esc(shortText(s.Name, 80)), esc(s.State), esc(s.SubState), esc(s.PID), esc(s.Restarts), esc(s.ExitCode), esc(s.Result))
		}
		rows = append(rows, []models.InlineKeyboardButton{{Text: "📄 " + shortText(s.Name, 40), CallbackData: "s:" + s.SourceID}})
	}
	if len(r.Services) == 0 {
		text += "Нет systemd-источников."
	}
	text += "\nСнимок: " + r.Time.In(loc).Format("15:04:05") + " · обновляется каждые 30с"
	rows = append(rows, pageButtons("services", p, len(r.Services))...)
	return text, rows
}

func (a *App) renderContainers(r monitor.Resources, opt string, loc *time.Location) (string, [][]models.InlineKeyboardButton) {
	if r.Time.IsZero() {
		return "Собираю состояние контейнеров. Нажмите «Обновить».", nil
	}
	text := "<b>Контейнеры из конфига</b>\n"
	if r.DockerError != "" {
		text += "⚠ " + esc(shortText(r.DockerError, 250)) + "\n"
	}
	p, start, end := pageIndex(opt, len(r.Containers))
	var rows [][]models.InlineKeyboardButton
	for _, c := range r.Containers[start:end] {
		icon := "🔴"
		if c.State == "running" && c.Health != "unhealthy" {
			icon = "🟢"
		}
		text += fmt.Sprintf("%s <b>%s</b> · %s %s\nCPU %s · RAM %s\nРестартов %d · exit %d · OOM %t\n", icon, esc(shortText(c.Name, 90)), esc(c.State), esc(c.Health), esc(c.CPU), esc(c.Memory), c.Restarts, c.ExitCode, c.OOMKilled)
		for _, s := range a.Config.Sources {
			if logs.Matches(s, logs.Container{Name: c.Name, Project: c.Project}) {
				rows = append(rows, []models.InlineKeyboardButton{{Text: "📄 " + shortText(c.Name, 40), CallbackData: "k:" + a.store.put(query{SourceID: s.ID, Target: c.Name})}})
				break
			}
		}
	}
	if len(r.Containers) == 0 && r.DockerError == "" {
		text += "Подходящих контейнеров нет."
	}
	text += "\nСнимок: " + r.Time.In(loc).Format("15:04:05") + " · обновляется каждые 30с"
	rows = append(rows, pageButtons("containers", p, len(r.Containers))...)
	return text, rows
}

func shortText(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return ' '
		}
		return r
	}, redact.Clean(s))
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
func size(n uint64) string {
	v := float64(n)
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	return fmt.Sprintf("%.1f%s", v, units[i])
}
func sparkline(values []float64) string {
	bars := []rune("▁▂▃▄▅▆▇█")
	var out strings.Builder
	for _, v := range values[max(0, len(values)-36):] {
		out.WriteRune(bars[max(0, min(7, int(v*7/100)))])
	}
	return out.String()
}
