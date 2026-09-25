// Package redact чистит строки логов перед отправкой в Telegram:
// убирает ANSI-коды и управляющие символы, маскирует похожее на секреты.
package redact

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxLineRunes = 1500

type rule struct {
	re   *regexp.Regexp
	repl string
}

// Порядок важен: сначала узкие правила, потом общее «ключ=значение».
var defaultRules = []rule{
	{regexp.MustCompile(`\b\d{8,10}:[A-Za-z0-9_-]{35}\b`), "<tg-token>"},
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s:@]+:[^/\s@]+@`), "${1}***:***@"},
	{regexp.MustCompile(`(?i)(authorization"?\s*[:=]\s*"?)(\w+\s+)?[^\s",;]+`), "${1}${2}***"},
	{regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{8,}`), "${1}***"},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), "<jwt>"},
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), "<aws-key>"},
	{regexp.MustCompile(`(?i)\b([a-z0-9_.-]*(?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|session[_-]?id|cookie)[a-z0-9_.-]*"?\s*[=:]\s*)("[^"]*"|'[^']*'|[^\s,;&"'}\]]+)`), "${1}***"},
}

var (
	reANSI = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\-_]`)
)

type Redactor struct {
	rules []rule
}

// New: disable выключает только маскирование секретов, чистка текста остаётся всегда.
func New(disable bool, extra []string) (*Redactor, error) {
	r := &Redactor{}
	if disable {
		return r, nil
	}
	r.rules = append(r.rules, defaultRules...)
	for _, e := range extra {
		re, err := regexp.Compile(e)
		if err != nil {
			return nil, err
		}
		r.rules = append(r.rules, rule{re, "***"})
	}
	return r, nil
}

func (r *Redactor) Mask(s string) string {
	for _, rl := range r.rules {
		s = rl.re.ReplaceAllString(s, rl.repl)
	}
	return s
}

// Clean делает строку пригодной для показа: валидный UTF-8, без ANSI и управляющих
// символов, табы — пробелами, длина ограничена.
func Clean(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = reANSI.ReplaceAllString(s, "")
	s = strings.TrimRight(s, "\r\n")
	if strings.IndexFunc(s, isControl) >= 0 {
		s = strings.Map(func(r rune) rune {
			switch {
			case r == '\t':
				return ' '
			case isControl(r):
				return -1
			}
			return r
		}, s)
	}
	if n := utf8.RuneCountInString(s); n > MaxLineRunes {
		rs := []rune(s)
		s = string(rs[:MaxLineRunes]) + "…"
	}
	return s
}

func isControl(r rune) bool { return r != ' ' && unicode.IsControl(r) }
