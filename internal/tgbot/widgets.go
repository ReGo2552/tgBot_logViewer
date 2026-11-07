package tgbot

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/ReGo2552/tgBot_logViewer/internal/monitor"
	"github.com/ReGo2552/tgBot_logViewer/internal/tgapi"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

//go:embed widget.html
var widgetHTML string

type widgetQuery struct {
	ID              string       `json:"id"`
	From            models.User  `json:"from"`
	Chat            *models.Chat `json:"chat"`
	MessageID       int          `json:"message_id"`
	InlineMessageID string       `json:"inline_message_id"`
	Data            string       `json:"data"`
}

func widgetData(s monitor.Snapshot, loc *time.Location) string {
	v := struct {
		Host    string    `json:"h"`
		Time    string    `json:"t"`
		CPU     int       `json:"cpu"`
		RAM     int       `json:"ram"`
		Disk    int       `json:"disk"`
		History []float64 `json:"hist,omitempty"`
		Note    string    `json:"note,omitempty"`
	}{Host: shortText(s.Host, 60), Time: s.Time.In(loc).Format("15:04:05"), CPU: -1, RAM: -1, Disk: -1}
	if s.CPUReady {
		v.CPU = int(math.Round(s.CPU))
	}
	if s.MemoryReady {
		v.RAM = int(math.Round(s.RAMPercent()))
	}
	for _, d := range s.Disks {
		if d.Path == "/" {
			v.Disk = int(math.Round(d.Percent))
		}
	}
	v.History = s.History[max(0, len(s.History)-24):]
	if s.Time.IsZero() {
		v.Note = "Собираю первый снимок. Нажмите «Обновить»."
	} else if len(s.Warnings) > 0 {
		v.Note = "Часть данных недоступна. Подробности: /status"
	} else if time.Since(s.Time) > time.Minute {
		v.Note = "Данные устарели"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func (a *App) sendWidget(ctx context.Context, b *bot.Bot, chatID int64) {
	if !a.Config.Widgets.Enabled || a.widgetDisabled.Load() {
		a.send(ctx, b, chatID, "Виджеты выключены или недоступны. Эксперимент включается через widgets.enabled в конфиге.", nil)
		a.panel(ctx, b, chatID, nil, "status")
		return
	}
	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err := a.api.Call(tctx, "sendWidget", map[string]any{
		"chat_id": chatID, "html": widgetHTML, "width": 320, "height": 310,
		"allow_queries": true, "is_shared": false, "chat_data": widgetData(a.monitor.Snapshot(), a.Config.Location),
	}, nil)
	if err != nil {
		a.Log.Warn("sendWidget", "err", tgapi.Scrub(err, a.Env.Token))
		a.send(ctx, b, chatID, "Telegram не принял виджет. Проверьте поддержку API и клиента. Показываю обычную панель.", nil)
		a.panel(ctx, b, chatID, nil, "status")
	}
}

func widgetAllowed(q widgetQuery, isAdmin func(int64) bool) bool {
	return isAdmin(q.From.ID) && q.Chat != nil && q.Chat.Type == models.ChatTypePrivate && q.Chat.ID == q.From.ID && q.MessageID > 0 && q.InlineMessageID == ""
}

func (a *App) handleWidget(ctx context.Context, b *bot.Bot, q widgetQuery) {
	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	answer := map[string]any{"widget_query_id": q.ID}
	if !widgetAllowed(q, a.Config.IsAdmin) {
		a.Log.Warn("отказ: widget query", "user_id", q.From.ID)
		if err := a.api.Call(tctx, "answerWidgetQuery", answer, nil); err != nil {
			a.Log.Warn("answerWidgetQuery", "err", tgapi.Scrub(err, a.Env.Token))
		}
		return
	}
	var action string
	switch q.Data {
	case "monitor:refresh":
		data := widgetData(a.monitor.Snapshot(), a.Config.Location)
		// A query answer is private to its requester; chat_data persists the snapshot.
		answer["data"] = data
		editCtx, editCancel := context.WithTimeout(tctx, 3*time.Second)
		err := a.api.Call(editCtx, "editMessageWidgetData", map[string]any{"chat_id": q.Chat.ID, "message_id": q.MessageID, "chat_data": data}, nil)
		editCancel()
		if err != nil && !strings.Contains(err.Error(), "message is not modified") {
			a.Log.Warn("editMessageWidgetData", "err", tgapi.Scrub(err, a.Env.Token))
		}
	case "monitor:top":
		action = "top"
	case "monitor:services":
		action = "services"
	case "monitor:logs":
		action = "logs"
	}
	if err := a.api.Call(tctx, "answerWidgetQuery", answer, nil); err != nil {
		a.Log.Warn("answerWidgetQuery", "err", tgapi.Scrub(err, a.Env.Token))
	}
	if action == "logs" {
		a.menu(ctx, b, q.Chat.ID, nil, "")
	} else if action != "" {
		a.panel(ctx, b, q.Chat.ID, nil, action)
	}
}

// pollWithWidgets replaces (never accompanies) SDK polling, preserving unknown
// widget_query fields. Normal updates still use the existing handler and models.
func (a *App) pollWithWidgets(ctx context.Context, b *bot.Bot) {
	var wg sync.WaitGroup
	normal := make(chan *models.Update, 100)
	widgets := make(chan widgetQuery, 100)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case u := <-normal:
					a.handle(ctx, b, u)
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case q := <-widgets:
				a.handleWidget(ctx, b, q)
			}
		}
	}()
	defer wg.Wait()
	var offset int64
	delay := time.Duration(0)
	for ctx.Err() == nil {
		if delay > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
		}
		allowed := []string{"message", "callback_query"}
		if !a.widgetDisabled.Load() {
			allowed = append(allowed, "widget_query")
		}
		var updates []json.RawMessage
		err := a.api.Call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": 40, "allowed_updates": allowed}, &updates)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			a.Log.Warn("getUpdates", "err", tgapi.Scrub(err, a.Env.Token))
			delay = 3 * time.Second
			var apiErr *tgapi.APIError
			if errors.As(err, &apiErr) {
				if apiErr.RetryAfter > 0 {
					delay = time.Duration(apiErr.RetryAfter) * time.Second
				}
				if apiErr.Code == 400 && !a.widgetDisabled.Load() {
					a.widgetDisabled.Store(true)
					delay = 0
					a.Log.Warn("виджеты отключены: getUpdates отклонил новые типы событий; продолжаю обычный polling")
				}
			}
			continue
		}
		delay = 0
		for _, raw := range updates {
			var head struct {
				ID     int64        `json:"update_id"`
				Widget *widgetQuery `json:"widget_query"`
			}
			err := json.Unmarshal(raw, &head)
			if head.ID == 0 {
				delay = 3 * time.Second
				continue
			}
			offset = head.ID + 1
			if err != nil {
				a.Log.Warn("пропущен повреждённый update", "id", head.ID)
				continue
			}
			if head.Widget != nil {
				select {
				case widgets <- *head.Widget:
				case <-ctx.Done():
					return
				}
			} else {
				var u models.Update
				if err := json.Unmarshal(raw, &u); err != nil {
					a.Log.Warn("пропущен повреждённый update", "id", head.ID)
					continue
				}
				select {
				case normal <- &u:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}
