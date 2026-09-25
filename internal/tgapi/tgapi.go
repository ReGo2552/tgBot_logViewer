// Package tgapi — HTTP-клиент до Bot API (с прокси) и проверка токена.
package tgapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const DefaultAPIURL = "https://api.telegram.org"

// PollTimeout — long polling getUpdates; клиенту даём запас сверху на медленный прокси.
const PollTimeout = 50 * time.Second

var reToken = regexp.MustCompile(`^\d{5,15}:[A-Za-z0-9_-]{30,}$`)

func ValidToken(t string) bool { return reToken.MatchString(t) }

// NewClient: proxy — пусто или http(s)://, socks5://, socks5h:// (DNS на стороне прокси).
func NewClient(proxy string) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil // HTTPS_PROXY из окружения не подхватываем: прокси задаётся только явно
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("PROXY_URL: %w", err)
		}
		switch u.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return nil, fmt.Errorf("PROXY_URL: схема %q не поддерживается (http, https, socks5, socks5h)", u.Scheme)
		}
		if u.Host == "" {
			return nil, errors.New("PROXY_URL: не указан хост")
		}
		tr.Proxy = http.ProxyURL(u)
	}
	tr.TLSHandshakeTimeout = 20 * time.Second
	return &http.Client{Transport: tr, Timeout: PollTimeout + 20*time.Second}, nil
}

// GetMe проверяет токен и связь; возвращает @username бота.
func GetMe(ctx context.Context, c *http.Client, apiURL, token string) (string, error) {
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(apiURL, "/")+"/bot"+token+"/getMe", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", scrub(err, token)
	}
	defer resp.Body.Close()
	var r struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			Username string `json:"username"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", fmt.Errorf("HTTP %d: %w", resp.StatusCode, err)
	}
	if !r.OK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, r.Description)
	}
	return r.Result.Username, nil
}

// scrub убирает токен из текста ошибки: net/http кладёт в неё полный URL.
func scrub(err error, token string) error {
	if token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), token, "<token>"))
}

// Scrub — то же для ошибок библиотеки бота.
func Scrub(err error, token string) error {
	if err == nil {
		return nil
	}
	return scrub(err, token)
}
