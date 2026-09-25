package logs

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ReGo2552/tgBot_logViewer/internal/config"
)

// MaxFileTailBytes — дальше этого от конца файла не читаем, даже если строк не хватило.
const MaxFileTailBytes = 16 << 20

// TailFile читает файл с конца, пока не наберёт n строк.
func TailFile(p string, n int) ([]Line, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}

	const chunk = 64 << 10
	var buf []byte
	pos := size
	for pos > 0 && size-pos < MaxFileTailBytes {
		step := min(int64(chunk), pos)
		pos -= step
		part := make([]byte, step)
		if _, err := f.ReadAt(part, pos); err != nil && err != io.EOF {
			return nil, err
		}
		buf = append(part, buf...)
		// n строк гарантированно есть, если переводов строк больше n (последний может завершать файл)
		if bytes.Count(buf, []byte{'\n'}) > n {
			break
		}
	}
	text := strings.TrimRight(string(buf), "\n")
	if text == "" {
		return nil, nil
	}
	parts := strings.Split(text, "\n")
	if pos > 0 {
		parts = parts[1:] // первая строка могла начаться до прочитанного куска
	}
	parts = lastN(parts, n)
	lines := make([]Line, len(parts))
	for i, s := range parts {
		lines[i] = Line{Text: s}
	}
	return lines, nil
}

// ResolveFiles раскрывает path источника в список обычных файлов (для glob — отсортированный).
func ResolveFiles(s config.Source) ([]string, error) {
	if !config.HasGlob(s.Path) {
		return []string{s.Path}, nil
	}
	matches, err := filepath.Glob(s.Path)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, m := range matches {
		if st, err := os.Stat(m); err == nil && st.Mode().IsRegular() {
			files = append(files, m)
		}
	}
	sort.Strings(files)
	return files, nil
}
