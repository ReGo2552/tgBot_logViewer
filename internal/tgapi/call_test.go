package tgapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCallEndpointAndErrors(t *testing.T) {
	const token = "123:secret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/beta/bot"+token+"/test/sendWidget" || r.Method != "POST" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		var p map[string]any
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Error(err)
		}
		if p["chat_id"] != float64(42) {
			t.Error("params", p)
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":7}}`)
	}))
	defer srv.Close()
	a := API{Client: srv.Client(), BaseURL: srv.URL + "/beta", Token: token, Test: true}
	var result struct {
		ID int `json:"message_id"`
	}
	if err := a.Call(context.Background(), "sendWidget", map[string]any{"chat_id": 42}, &result); err != nil || result.ID != 7 {
		t.Fatalf("%+v %v", result, err)
	}
	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		fmt.Fprintf(w, `{"ok":false,"error_code":429,"description":"token %s","parameters":{"retry_after":17}}`, token)
	}))
	defer failed.Close()
	a.BaseURL = failed.URL
	err := a.Call(context.Background(), "getUpdates", nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.RetryAfter != 17 || strings.Contains(err.Error(), token) {
		t.Fatal(err)
	}
}
