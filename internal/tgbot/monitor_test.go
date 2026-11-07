package tgbot

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ReGo2552/tgBot_logViewer/internal/config"
	"github.com/ReGo2552/tgBot_logViewer/internal/logs"
	"github.com/ReGo2552/tgBot_logViewer/internal/monitor"
	"github.com/ReGo2552/tgBot_logViewer/internal/tgapi"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

func TestObservationsUseFreshSamplesAndAllowlistedSources(t *testing.T) {
	cfg, err := config.Parse([]byte(`alerts: {watch_sources: [app, stack]}
sources:
  - {id: app, type: systemd, unit: app}
  - {id: stack, type: docker, match: "stack:app"}
`))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s := monitor.Snapshot{Time: now, CPUReady: true, CPU: 95, MemoryReady: true, MemoryTotal: 100, MemoryUsed: 88}
	r := monitor.Resources{Time: now, Services: []monitor.Service{{SourceID: "app", State: "failed", SubState: "failed", ExitCode: "1"}}, Containers: []logs.ContainerStatus{{Name: "web", Project: "app", State: "running", Health: "unhealthy"}, {Name: "other", Project: "private", State: "exited"}}}
	obs := observations(cfg, s, r, now)
	if len(obs) != 4 || !obs[0].Bad || obs[1].Bad || obs[1].Clear || !obs[2].Bad || !obs[3].Bad {
		t.Fatalf("%+v", obs)
	}
	r.DockerError = "permission denied"
	r.Services[0].Error = "unavailable"
	if obs := observations(cfg, s, r, now); len(obs) != 2 {
		t.Fatal("backend errors treated as recovery", obs)
	}
	if obs := observations(cfg, s, r, now.Add(time.Hour)); len(obs) != 0 {
		t.Fatal("stale data evaluated", obs)
	}
	r.Services[0].Missing = true
	if obs := observations(cfg, s, r, now); len(obs) != 3 || !obs[2].Bad {
		t.Fatal("missing service was not detected", obs)
	}
}

func TestWidgetSizePayloadAndAccess(t *testing.T) {
	var packed bytes.Buffer
	z, _ := zlib.NewWriterLevel(&packed, zlib.BestCompression)
	z.Write([]byte(widgetHTML))
	z.Close()
	if len(widgetHTML) > 32768 || packed.Len() > 4096 {
		t.Fatalf("widget too large: %d / %d", len(widgetHTML), packed.Len())
	}
	s := monitor.Snapshot{Host: strings.Repeat("<&я", 100), Time: time.Now(), CPU: 95, CPUReady: true, History: make([]float64, 120)}
	data := widgetData(s, time.UTC)
	if len([]byte(data)) > 1024 || !json.Valid([]byte(data)) {
		t.Fatal("invalid widget state", len(data))
	}
	admin := func(id int64) bool { return id == 42 }
	q := widgetQuery{From: models.User{ID: 42}, Chat: &models.Chat{ID: 42, Type: models.ChatTypePrivate}, MessageID: 7}
	if !widgetAllowed(q, admin) {
		t.Fatal("valid original rejected")
	}
	for _, change := range []func(*widgetQuery){func(q *widgetQuery) { q.From.ID = 99 }, func(q *widgetQuery) { q.Chat = nil }, func(q *widgetQuery) { q.Chat = &models.Chat{ID: 42, Type: models.ChatTypeGroup} }, func(q *widgetQuery) { q.Chat = &models.Chat{ID: 99, Type: models.ChatTypePrivate} }, func(q *widgetQuery) { q.InlineMessageID = "inline" }} {
		copy := q
		change(&copy)
		if widgetAllowed(copy, admin) {
			t.Fatal("unsafe widget request allowed")
		}
	}
}

func TestWidgetAndNormalUpdatesShareOnePoller(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	polls := 0
	got := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		switch method {
		case "getUpdates":
			mu.Lock()
			polls++
			first := polls == 1
			mu.Unlock()
			if first {
				fmt.Fprint(w, `{"ok":true,"result":[{"update_id":1,"message":{"message_id":10,"from":{"id":42},"chat":{"id":42,"type":"private"},"text":"/status"}},{"update_id":2,"widget_query":{"id":"q","from":{"id":42},"chat":{"id":42,"type":"private"},"message_id":7,"data":"monitor:refresh"}}]}`)
			} else {
				<-ctx.Done()
			}
		case "sendMessage":
			got <- method
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":11,"chat":{"id":42,"type":"private"}}}`)
		case "editMessageWidgetData", "answerWidgetQuery":
			got <- method
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		default:
			t.Errorf("unexpected method %s", method)
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		}
	}))
	defer srv.Close()
	cfg, _ := config.Parse([]byte("admins: [42]\nwidgets: {enabled: true}"))
	a := &App{Options: Options{Config: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, monitor: monitor.New(nil), resources: &monitor.ResourceCache{}, api: &tgapi.API{Client: srv.Client(), BaseURL: srv.URL, Token: "123:secret"}}
	b, err := bot.New("123:secret", bot.WithSkipGetMe(), bot.WithServerURL(srv.URL), bot.WithHTTPClient(time.Second, srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { a.pollWithWidgets(ctx, b); close(done) }()
	seen := map[string]bool{}
	for len(seen) < 3 {
		select {
		case m := <-got:
			seen[m] = true
		case <-time.After(3 * time.Second):
			cancel()
			t.Fatal("missing requests", seen)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("polling did not stop")
	}
}

func TestRenderTopEscapesNamesAndPreservesOrder(t *testing.T) {
	s := monitor.Snapshot{Time: time.Now(), CPUReady: true, Processes: []monitor.Process{{PID: 1, Name: "<script>", User: "root", CPU: 1, RSS: 200}, {PID: 2, Name: "worker", User: "app", CPU: 20, RSS: 100}}}
	text := renderTop(s, "cpu", 10, time.UTC)
	if strings.Index(text, "worker") > strings.Index(text, "&lt;script&gt;") || strings.Contains(text, "<script>") || s.Processes[0].PID != 1 {
		t.Fatal(text)
	}
}

func TestRunExistingPollingAndMonitoringLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	sentUpdate := false
	status := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		switch r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:] {
		case "getMe":
			fmt.Fprint(w, `{"ok":true,"result":{"id":123,"is_bot":true,"first_name":"test","username":"test_bot"}}`)
		case "setMyCommands":
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		case "getUpdates":
			mu.Lock()
			first := !sentUpdate
			sentUpdate = true
			mu.Unlock()
			if first {
				fmt.Fprint(w, `{"ok":true,"result":[{"update_id":1,"message":{"message_id":1,"from":{"id":42},"chat":{"id":42,"type":"private"},"text":"/status"}}]}`)
			} else {
				<-ctx.Done()
			}
		case "sendMessage":
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":2,"chat":{"id":42,"type":"private"}}}`)
			status <- struct{}{}
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	cfg, _ := config.Parse([]byte("admins: [42]"))
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Config: cfg, Env: config.Env{Token: "123:secret", APIURL: srv.URL}, Fetcher: &logs.Fetcher{Docker: logs.NewDocker(cfg)}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	}()
	select {
	case <-status:
	case err := <-done:
		cancel()
		t.Fatal("early exit", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("no status response")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("background workers did not stop")
	}
}

func TestWidgetFallbackAndNoChangeAnswer(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		methods = append(methods, method)
		switch method {
		case "sendWidget":
			w.WriteHeader(404)
			fmt.Fprint(w, `{"ok":false,"error_code":404,"description":"Not Found"}`)
		case "sendMessage":
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":3,"chat":{"id":42,"type":"private"}}}`)
		case "editMessageWidgetData":
			w.WriteHeader(400)
			fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"message is not modified"}`)
		case "answerWidgetQuery":
			var p map[string]any
			json.NewDecoder(r.Body).Decode(&p)
			if p["widget_query_id"] == "refresh" {
				if data, ok := p["data"].(string); !ok || !json.Valid([]byte(data)) {
					t.Error("missing answer data")
				}
			} else if _, ok := p["data"]; ok {
				t.Error("data sent to unauthorized viewer")
			}
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		default:
			t.Error("unexpected", method)
		}
	}))
	defer srv.Close()
	cfg, _ := config.Parse([]byte("admins: [42]\nwidgets: {enabled: true}"))
	a := &App{Options: Options{Config: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, monitor: monitor.New(nil), resources: &monitor.ResourceCache{}, api: &tgapi.API{Client: srv.Client(), BaseURL: srv.URL, Token: "123:secret"}}
	b, _ := bot.New("123:secret", bot.WithSkipGetMe(), bot.WithServerURL(srv.URL), bot.WithHTTPClient(time.Second, srv.Client()))
	a.sendWidget(context.Background(), b, 42)
	if strings.Join(methods, ",") != "sendWidget,sendMessage,sendMessage" {
		t.Fatal("fallback", methods)
	}
	methods = nil
	a.handleWidget(context.Background(), b, widgetQuery{ID: "refresh", From: models.User{ID: 42}, Chat: &models.Chat{ID: 42, Type: models.ChatTypePrivate}, MessageID: 7, Data: "monitor:refresh"})
	if strings.Join(methods, ",") != "editMessageWidgetData,answerWidgetQuery" {
		t.Fatal("no-change must still answer", methods)
	}
	methods = nil
	a.handleWidget(context.Background(), b, widgetQuery{ID: "denied", From: models.User{ID: 99}, Chat: &models.Chat{ID: 99, Type: models.ChatTypePrivate}, MessageID: 7, Data: "monitor:refresh"})
	if strings.Join(methods, ",") != "answerWidgetQuery" {
		t.Fatal("unsafe request reached metrics", methods)
	}
}

func TestUnsupportedWidgetUpdatesKeepOrdinaryCommandsWorking(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	polls := 0
	status := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if method == "sendMessage" {
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":3,"chat":{"id":42,"type":"private"}}}`)
			status <- struct{}{}
			return
		}
		if method != "getUpdates" {
			t.Error(method)
			return
		}
		var p struct {
			Allowed []string `json:"allowed_updates"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Error(err)
		}
		mu.Lock()
		polls++
		n := polls
		mu.Unlock()
		switch n {
		case 1:
			w.WriteHeader(400)
			fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Unsupported update type"}`)
		case 2:
			for _, typ := range p.Allowed {
				if typ == "widget_query" {
					t.Error("unsupported update type was not removed")
				}
			}
			fmt.Fprint(w, `{"ok":true,"result":[{"update_id":1,"message":{"message_id":1,"from":{"id":42},"chat":{"id":42,"type":"private"},"text":"/status"}}]}`)
		default:
			<-ctx.Done()
		}
	}))
	defer srv.Close()
	cfg, _ := config.Parse([]byte("admins: [42]\nwidgets: {enabled: true}"))
	a := &App{Options: Options{Config: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, monitor: monitor.New(nil), resources: &monitor.ResourceCache{}, api: &tgapi.API{Client: srv.Client(), BaseURL: srv.URL, Token: "123:secret"}}
	b, _ := bot.New("123:secret", bot.WithSkipGetMe(), bot.WithServerURL(srv.URL), bot.WithHTTPClient(time.Second, srv.Client()))
	done := make(chan struct{})
	go func() { a.pollWithWidgets(ctx, b); close(done) }()
	select {
	case <-status:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("ordinary commands stopped after widget rejection")
	}
	cancel()
	<-done
	if !a.widgetDisabled.Load() {
		t.Fatal("widget mode was not disabled")
	}
}
