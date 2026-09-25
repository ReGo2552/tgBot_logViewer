// Package logs достаёт последние строки из journald, docker и файлов.
package logs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Line — одна строка лога. Time нулевое, если источник время не даёт (файлы).
type Line struct {
	Time time.Time `json:"t,omitzero"`
	Text string    `json:"s"`
}

// MaxOutputBytes — сколько вывода команды держим в памяти; остальное (самое старое)
// отбрасывается.
const MaxOutputBytes = 16 << 20

// CommandFunc создаёт команду. Подменяется, чтобы выполнить её от другого
// пользователя (doctor от root проверяет права logbot) или в тестах.
type CommandFunc func(ctx context.Context, name string, args ...string) *exec.Cmd

func DefaultCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "SYSTEMD_COLORS=0", "SYSTEMD_PAGER=", "PAGER=cat", "LC_ALL=C.UTF-8")
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

// tailBuffer хранит только последние limit байт записанного.
type tailBuffer struct {
	buf       []byte
	limit     int
	truncated bool
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2*t.limit {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.limit:]...)
		t.truncated = true
	}
	return len(p), nil
}

// Bytes возвращает хвост, начиная с целой строки.
func (t *tailBuffer) Bytes() []byte {
	b := t.buf
	if len(b) > t.limit {
		b = b[len(b)-t.limit:]
		t.truncated = true
	}
	if t.truncated {
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	return b
}

type output struct {
	stdout, stderr []byte
}

func runCmd(ctx context.Context, mk CommandFunc, name string, args ...string) (output, error) {
	out := &tailBuffer{limit: MaxOutputBytes}
	errb := &tailBuffer{limit: 64 << 10}
	cmd := mk(ctx, name, args...)
	cmd.Stdout = out
	cmd.Stderr = errb
	err := cmd.Run()
	o := output{stdout: out.Bytes(), stderr: errb.Bytes()}
	if err != nil {
		if ctx.Err() != nil {
			return o, fmt.Errorf("%s: не уложился во время (%w)", name, ctx.Err())
		}
		if msg := firstLine(o.stderr); msg != "" {
			return o, fmt.Errorf("%s: %s", name, msg)
		}
		return o, fmt.Errorf("%s: %w", name, err)
	}
	return o, nil
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	// sudo и docker иногда начинают с пустых предупреждений — берём первую содержательную
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			if len(l) > 300 {
				l = l[:300] + "…"
			}
			return l
		}
	}
	return ""
}

func lastN[T any](s []T, n int) []T {
	if n >= 0 && len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
