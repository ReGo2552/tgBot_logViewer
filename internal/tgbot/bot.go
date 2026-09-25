// Package tgbot — Telegram-интерфейс: меню источников, вывод логов, /discover, /doctor.
package tgbot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/ReGo2552/tgBot_logViewer/internal/config"
	"github.com/ReGo2552/tgBot_logViewer/internal/logs"
	"github.com/ReGo2552/tgBot_logViewer/internal/tgapi"
)

const (
	fetchTimeout  = 30 * time.Second
	reportTimeout = 90 * time.Second
	maxFilterLen  = 100
)

type Options struct {
	Config   *config.Config
	Env      config.Env
	Fetcher  *logs.Fetcher
	Doctor   func(ctx context.Context) string
	Discover func(ctx context.Context) (string, error)
	Log      *slog.Logger
}

type App struct {
	Options
	store *store
	sem   chan struct{} // не больше двух чтений логов одновременно
}

func Run(ctx context.Context, o Options) error {
	client, err := tgapi.NewClient(o.Env.Proxy)
	if err != nil {
		return err
	}
	a := &App{Options: o, store: newStore(2000), sem: make(chan struct{}, 2)}
	token := o.Env.Token

	opts := []bot.Option{
		bot.WithHTTPClient(tgapi.PollTimeout, client),
		bot.WithCheckInitTimeout(30 * time.Second),
		bot.WithDefaultHandler(a.handle),
		bot.WithAllowedUpdates(bot.AllowedUpdates{"message", "callback_query"}),
		bot.WithErrorsHandler(func(err error) {
			a.Log.Warn("telegram", "err", tgapi.Scrub(err, token))
		}),
	}
	if o.Env.APIURL != "" {
		opts = append(opts, bot.WithServerURL(o.Env.APIURL))
	}
	b, err := bot.New(token, opts...)
	if err != nil {
		return fmt.Errorf("подключение к Telegram: %w", tgapi.Scrub(err, token))
	}
	me, err := b.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("getMe: %w", tgapi.Scrub(err, token))
	}
	_, err = b.SetMyCommands(ctx, &bot.SetMyCommandsParams{Commands: []models.BotCommand{
		{Command: "logs", Description: "Логи: выбрать источник"},
		{Command: "discover", Description: "Что ещё можно добавить в конфиг"},
		{Command: "doctor", Description: "Проверить доступ к источникам"},
		{Command: "help", Description: "Справка"},
	}})
	if err != nil {
		a.Log.Warn("setMyCommands", "err", tgapi.Scrub(err, token))
	}
	a.Log.Info("бот запущен", "username", me.Username, "sources", len(o.Config.Sources), "admins", len(o.Config.Admins))
	b.Start(ctx)
	return nil
}

// ---------------------------------------------------------------- маршрутизация

func (a *App) handle(ctx context.Context, b *bot.Bot, upd *models.Update) {
	switch {
	case upd.Message != nil:
		m := upd.Message
		if m.From == nil || !a.allowed(m.From, m.Chat.Type) {
			a.denied(m.From, m.Chat.Type, m.Text)
			return
		}
		a.onMessage(ctx, b, m)
	case upd.CallbackQuery != nil:
		cq := upd.CallbackQuery
		chatType := models.ChatTypePrivate
		if msg := cq.Message.Message; msg != nil {
			chatType = msg.Chat.Type
		}
		if !a.allowed(&cq.From, chatType) {
			a.denied(&cq.From, chatType, "callback "+cq.Data)
			b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cq.ID})
			return
		}
		a.onCallback(ctx, b, cq)
	}
}

// allowed: только админы из конфига и только в личке — в группе логи увидят все участники.
func (a *App) allowed(u *models.User, chat models.ChatType) bool {
	return u != nil && chat == models.ChatTypePrivate && a.Config.IsAdmin(u.ID)
}

func (a *App) denied(u *models.User, chat models.ChatType, text string) {
	var id int64
	var name string
	if u != nil {
		id, name = u.ID, u.Username
	}
	if r := []rune(text); len(r) > 60 {
		text = string(r[:60]) + "…"
	}
	a.Log.Warn("отказ: чужой пользователь или не личный чат", "user_id", id, "username", name, "chat", chat, "text", text)
}

func (a *App) onMessage(ctx context.Context, b *bot.Bot, m *models.Message) {
	chatID := m.Chat.ID
	fields := strings.Fields(m.Text)
	if len(fields) == 0 {
		return
	}
	cmd := fields[0]
	if !strings.HasPrefix(cmd, "/") {
		// «caddy 200» без команды — то же, что /logs caddy 200
		a.logsCmd(ctx, b, chatID, fields)
		return
	}
	cmd, _, _ = strings.Cut(cmd, "@")
	switch cmd {
	case "/start", "/help":
		a.send(ctx, b, chatID, helpText, nil)
	case "/logs":
		if len(fields) == 1 {
			a.menu(ctx, b, chatID, nil, "")
		} else {
			a.logsCmd(ctx, b, chatID, fields[1:])
		}
	case "/discover":
		a.report(ctx, b, chatID, "discover.yaml", func(ctx context.Context) (string, error) { return a.Discover(ctx) })
	case "/doctor":
		a.report(ctx, b, chatID, "doctor.txt", func(ctx context.Context) (string, error) { return a.Doctor(ctx), nil })
	default:
		a.send(ctx, b, chatID, "Не знаю такой команды. /help", nil)
	}
}

func (a *App) onCallback(ctx context.Context, b *bot.Bot, cq *models.CallbackQuery) {
	b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cq.ID})
	msg := cq.Message.Message // nil, если сообщение слишком старое
	chatID := cq.From.ID
	if msg != nil {
		chatID = msg.Chat.ID
	}

	kind, arg, _ := strings.Cut(cq.Data, ":")
	switch kind {
	case "m":
		a.menu(ctx, b, chatID, msg, arg)
	case "s":
		s, ok := a.Config.Source(arg)
		if !ok {
			a.send(ctx, b, chatID, "Источника больше нет в конфиге. /logs", nil)
			return
		}
		q := query{SourceID: s.ID}
		if s.IsMulti() {
			a.targets(ctx, b, chatID, msg, s, q)
		} else {
			a.show(ctx, b, chatID, nil, q)
		}
	case "k":
		q, ok := a.store.get(arg)
		if !ok {
			a.send(ctx, b, chatID, "Кнопка устарела (бот перезапускался). /logs", nil)
			return
		}
		a.show(ctx, b, chatID, msg, q)
	}
}

// ---------------------------------------------------------------- меню

func (a *App) menu(ctx context.Context, b *bot.Bot, chatID int64, msg *models.Message, typ string) {
	counts := map[string]int{}
	for _, s := range a.Config.Sources {
		counts[s.Type]++
	}
	if len(a.Config.Sources) == 0 {
		a.send(ctx, b, chatID, "В конфиге нет источников. Посмотрите /discover.", nil)
		return
	}
	if typ == "" && len(counts) == 1 {
		for t := range counts {
			typ = t
		}
	}

	var rows [][]models.InlineKeyboardButton
	var text string
	if typ == "" {
		text = "<b>Логи</b>: выберите раздел"
		for _, t := range typeOrder {
			if counts[t] > 0 {
				rows = append(rows, []models.InlineKeyboardButton{{
					Text: fmt.Sprintf("%s (%d)", typeTitles[t], counts[t]), CallbackData: "m:" + t,
				}})
			}
		}
	} else {
		text = "<b>" + typeTitles[typ] + "</b>: выберите источник"
		var row []models.InlineKeyboardButton
		for _, s := range a.Config.Sources {
			if s.Type != typ {
				continue
			}
			label := s.Label()
			if s.IsMulti() {
				label += " ›"
			}
			row = append(row, models.InlineKeyboardButton{Text: label, CallbackData: "s:" + s.ID})
			if len(row) == 2 {
				rows, row = append(rows, row), nil
			}
		}
		if row != nil {
			rows = append(rows, row)
		}
		if len(counts) > 1 {
			rows = append(rows, []models.InlineKeyboardButton{{Text: "⬅️ Разделы", CallbackData: "m"}})
		}
	}
	a.sendOrEdit(ctx, b, chatID, msg, text, &models.InlineKeyboardMarkup{InlineKeyboard: rows})
}

// targets — выбор контейнера стека или файла из glob.
func (a *App) targets(ctx context.Context, b *bot.Bot, chatID int64, msg *models.Message, s config.Source, q query) {
	tctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	ts, err := a.Fetcher.Targets(tctx, s)
	if err != nil {
		a.send(ctx, b, chatID, fmt.Sprintf("❌ <b>%s</b>: %s", esc(s.Label()), esc(err.Error())), nil)
		return
	}
	switch len(ts) {
	case 0:
		a.send(ctx, b, chatID, fmt.Sprintf("<b>%s</b>: сейчас ничего не подходит", esc(s.Label())), nil)
		return
	case 1:
		q.Target = ts[0].Name
		a.show(ctx, b, chatID, nil, q)
		return
	}
	var rows [][]models.InlineKeyboardButton
	for _, t := range ts {
		tq := q
		tq.Target = t.Name
		rows = append(rows, []models.InlineKeyboardButton{{Text: t.Label, CallbackData: "k:" + a.store.put(tq)}})
	}
	rows = append(rows, []models.InlineKeyboardButton{{Text: "⬅️ Назад", CallbackData: "m:" + s.Type}})
	a.sendOrEdit(ctx, b, chatID, msg, fmt.Sprintf("<b>%s</b>: выберите", esc(s.Label())),
		&models.InlineKeyboardMarkup{InlineKeyboard: rows})
}

// logsCmd: /logs id [N] [фильтр…]. id может быть и именем контейнера из стека.
func (a *App) logsCmd(ctx context.Context, b *bot.Bot, chatID int64, args []string) {
	id := args[0]
	s, ok := a.Config.Source(id)
	target := ""
	if !ok {
		s, target, ok = a.findContainer(ctx, id)
	}
	if !ok {
		a.send(ctx, b, chatID, fmt.Sprintf("Источника «%s» нет. Список: /logs", esc(id)), nil)
		return
	}
	q := query{SourceID: s.ID, Target: target}
	rest := args[1:]
	if len(rest) > 0 {
		if n, err := strconv.Atoi(rest[0]); err == nil {
			q.Lines = n
			rest = rest[1:]
		}
	}
	q.Filter = strings.Join(rest, " ")
	if utf8.RuneCountInString(q.Filter) > maxFilterLen {
		q.Filter = string([]rune(q.Filter)[:maxFilterLen])
	}
	if q.Lines > a.Config.Lines.Max {
		a.send(ctx, b, chatID, fmt.Sprintf("Больше %d строк за раз не отдаю, покажу %d.", a.Config.Lines.Max, a.Config.Lines.Max), nil)
	}
	if s.IsMulti() && target == "" {
		a.targets(ctx, b, chatID, nil, s, q)
		return
	}
	a.show(ctx, b, chatID, nil, q)
}

func (a *App) findContainer(ctx context.Context, name string) (config.Source, string, bool) {
	if !a.Config.HasType(config.TypeDocker) || !config.ValidContainerName(name) {
		return config.Source{}, "", false
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	cs, err := a.Fetcher.Docker.List(ctx)
	if err != nil {
		return config.Source{}, "", false
	}
	for _, c := range cs {
		if c.Name != name {
			continue
		}
		for _, s := range a.Config.Sources {
			if logs.Matches(s, c) {
				return s, c.Name, true
			}
		}
	}
	return config.Source{}, "", false
}

// ---------------------------------------------------------------- вывод логов

func (a *App) show(ctx context.Context, b *bot.Bot, chatID int64, msg *models.Message, q query) {
	s, ok := a.Config.Source(q.SourceID)
	if !ok {
		a.send(ctx, b, chatID, "Источника больше нет в конфиге. /logs", nil)
		return
	}
	select {
	case a.sem <- struct{}{}:
		defer func() { <-a.sem }()
	case <-time.After(fetchTimeout):
		a.send(ctx, b, chatID, "Бот занят другими запросами, попробуйте ещё раз.", nil)
		return
	case <-ctx.Done():
		return
	}

	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	start := time.Now()
	res, err := a.Fetcher.Fetch(fctx, logs.Request{Source: s, Target: q.Target, Lines: q.Lines, Filter: q.Filter})
	a.Log.Info("запрос логов", "chat", chatID, "source", s.ID, "target", q.Target, "lines", q.Lines,
		"filter", q.Filter, "ms", time.Since(start).Milliseconds(), "err", err)
	if err != nil {
		a.send(ctx, b, chatID, fmt.Sprintf("❌ <b>%s</b>: %s", esc(s.Label()), esc(err.Error())), nil)
		return
	}

	loc := a.Config.Location
	head := header(s, q, res, loc)
	kb := a.resultKeyboard(q)
	if len(res.Lines) == 0 {
		a.sendOrEditIf(ctx, b, chatID, msg, q.Edit, head+"\n\n<i>пусто</i>", kb)
		return
	}
	text := body(res.Lines, loc)
	if !q.AsFile && fitsInline(text) {
		a.sendOrEditIf(ctx, b, chatID, msg, q.Edit, head+"\n<pre>"+esc(text)+"</pre>", kb)
		return
	}

	data := []byte(text + "\n")
	caption := head + fmt.Sprintf("\n📎 %.1f КБ", float64(len(data))/1024)
	_, err = b.SendDocument(ctx, &bot.SendDocumentParams{
		ChatID:      chatID,
		Document:    &models.InputFileUpload{Filename: fileName(s, res, time.Now().In(loc)), Data: bytes.NewReader(data)},
		Caption:     caption,
		ParseMode:   models.ParseModeHTML,
		ReplyMarkup: kb,
	})
	if err != nil {
		a.Log.Warn("sendDocument", "err", tgapi.Scrub(err, a.Env.Token))
	}
}

var counts = []int{50, 100, 200, 500, 1000}

func (a *App) resultKeyboard(q query) *models.InlineKeyboardMarkup {
	cur := a.Config.ClampLines(q.Lines)
	var row1 []models.InlineKeyboardButton
	for _, n := range counts {
		if n == cur || n > a.Config.Lines.Max {
			continue
		}
		nq := q
		nq.Lines, nq.AsFile, nq.Edit = n, false, true
		row1 = append(row1, models.InlineKeyboardButton{Text: strconv.Itoa(n), CallbackData: "k:" + a.store.put(nq)})
	}
	rq := q
	rq.Edit = true
	row2 := []models.InlineKeyboardButton{{Text: "🔄 Обновить", CallbackData: "k:" + a.store.put(rq)}}
	if !q.AsFile {
		fq := q
		fq.AsFile, fq.Edit = true, false
		row2 = append(row2, models.InlineKeyboardButton{Text: "📎 Файлом", CallbackData: "k:" + a.store.put(fq)})
	}
	row2 = append(row2, models.InlineKeyboardButton{Text: "☰ Меню", CallbackData: "m"})
	return &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{row1, row2}}
}

// report — /discover и /doctor: текст моноширинным, длинный — файлом.
func (a *App) report(ctx context.Context, b *bot.Bot, chatID int64, name string, fn func(context.Context) (string, error)) {
	rctx, cancel := context.WithTimeout(ctx, reportTimeout)
	defer cancel()
	text, err := fn(rctx)
	if err != nil {
		a.send(ctx, b, chatID, "❌ "+esc(err.Error()), nil)
		return
	}
	if fitsInline(text) {
		a.send(ctx, b, chatID, "<pre>"+esc(text)+"</pre>", nil)
		return
	}
	_, err = b.SendDocument(ctx, &bot.SendDocumentParams{
		ChatID:   chatID,
		Document: &models.InputFileUpload{Filename: name, Data: strings.NewReader(text)},
	})
	if err != nil {
		a.Log.Warn("sendDocument", "err", tgapi.Scrub(err, a.Env.Token))
	}
}

// ---------------------------------------------------------------- отправка

func (a *App) send(ctx context.Context, b *bot.Bot, chatID int64, text string, kb models.ReplyMarkup) {
	_, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:             chatID,
		Text:               text,
		ParseMode:          models.ParseModeHTML,
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: bot.True()},
		ReplyMarkup:        kb,
	})
	if err != nil {
		a.Log.Warn("sendMessage", "err", tgapi.Scrub(err, a.Env.Token))
	}
}

func (a *App) sendOrEditIf(ctx context.Context, b *bot.Bot, chatID int64, msg *models.Message, edit bool, text string, kb models.ReplyMarkup) {
	if !edit {
		msg = nil
	}
	a.sendOrEdit(ctx, b, chatID, msg, text, kb)
}

// sendOrEdit правит текстовое сообщение на месте; документ отредактировать в текст нельзя,
// поэтому для него — новое сообщение.
func (a *App) sendOrEdit(ctx context.Context, b *bot.Bot, chatID int64, msg *models.Message, text string, kb models.ReplyMarkup) {
	if msg == nil || msg.Document != nil || msg.Text == "" {
		a.send(ctx, b, chatID, text, kb)
		return
	}
	_, err := b.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID:             chatID,
		MessageID:          msg.ID,
		Text:               text,
		ParseMode:          models.ParseModeHTML,
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: bot.True()},
		ReplyMarkup:        kb,
	})
	if err == nil || strings.Contains(err.Error(), "message is not modified") {
		return
	}
	var tooMany *bot.TooManyRequestsError
	if errors.As(err, &tooMany) {
		a.Log.Warn("editMessageText: лимит Telegram", "retry_after", tooMany.RetryAfter)
		return
	}
	// сообщение удалили или оно слишком старое — присылаем новое
	a.Log.Info("editMessageText не удался, шлю новое", "err", tgapi.Scrub(err, a.Env.Token))
	a.send(ctx, b, chatID, text, kb)
}
