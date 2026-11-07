package tgapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// API handles methods not yet represented by the pinned Telegram SDK.
// It uses the same HTTP client, proxy and endpoint as ordinary bot requests.
type API struct {
	Client         *http.Client
	BaseURL, Token string
	Test           bool
}

type APIError struct {
	Code        int
	Description string
	RetryAfter  int
}

func (e *APIError) Error() string { return fmt.Sprintf("Telegram %d: %s", e.Code, e.Description) }

func (a *API) Call(ctx context.Context, method string, params, result any) error {
	base := a.BaseURL
	if base == "" {
		base = DefaultAPIURL
	}
	endpoint := strings.TrimRight(base, "/") + "/bot" + a.Token + "/"
	if a.Test {
		endpoint += "test/"
	}
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+method, bytes.NewReader(b))
	if err != nil {
		return Scrub(err, a.Token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.Client.Do(req)
	if err != nil {
		return Scrub(err, a.Token)
	}
	defer resp.Body.Close()
	var envelope struct {
		OK          bool            `json:"ok"`
		Code        int             `json:"error_code"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 20<<20)).Decode(&envelope); err != nil {
		return fmt.Errorf("Telegram response HTTP %d: %w", resp.StatusCode, err)
	}
	if !envelope.OK || resp.StatusCode != http.StatusOK {
		return &APIError{Code: envelope.Code, Description: strings.ReplaceAll(envelope.Description, a.Token, "<token>"), RetryAfter: envelope.Parameters.RetryAfter}
	}
	if result != nil {
		return json.Unmarshal(envelope.Result, result)
	}
	return nil
}
