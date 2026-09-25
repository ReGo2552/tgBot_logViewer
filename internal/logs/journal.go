package logs

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Journal — последние n записей юнита из journald.
func Journal(ctx context.Context, mk CommandFunc, unit string, n int) ([]Line, error) {
	o, err := runCmd(ctx, mk, "journalctl",
		"--unit="+unit, "--lines="+strconv.Itoa(n),
		"--output=json", "--output-fields=MESSAGE",
		"--no-pager", "--quiet")
	if err != nil {
		return nil, err
	}
	return parseJournal(o.stdout, n), nil
}

type journalEntry struct {
	Realtime string          `json:"__REALTIME_TIMESTAMP"`
	Message  json.RawMessage `json:"MESSAGE"`
}

func parseJournal(data []byte, n int) []Line {
	var lines []Line
	for _, raw := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var e journalEntry
		if json.Unmarshal(raw, &e) != nil {
			continue // обрезанная первая строка после tailBuffer
		}
		var t time.Time
		if us, err := strconv.ParseInt(e.Realtime, 10, 64); err == nil {
			t = time.UnixMicro(us)
		}
		for _, text := range strings.Split(journalMessage(e.Message), "\n") {
			lines = append(lines, Line{Time: t, Text: text})
		}
	}
	return lastN(lines, n)
}

// journalMessage: MESSAGE бывает строкой, массивом байт (не-UTF-8 или бинарное) или null.
func journalMessage(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	switch raw[0] {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	case '[':
		var ints []int
		if json.Unmarshal(raw, &ints) == nil {
			b := make([]byte, len(ints))
			for i, v := range ints {
				b[i] = byte(v)
			}
			return string(b)
		}
	}
	return ""
}
