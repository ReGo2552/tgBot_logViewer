package tgbot

import (
	"strconv"
	"sync"
)

// query — что показать. Хранится в памяти, в кнопку уходит только короткий ключ:
// callback_data ограничена 64 байтами, а имя контейнера, путь и фильтр туда не влезут.
type query struct {
	SourceID string
	Target   string
	Lines    int
	Filter   string
	AsFile   bool
	Edit     bool // заменить текущее сообщение, а не присылать новое
}

type store struct {
	mu    sync.Mutex
	m     map[string]query
	order []string
	next  uint64
	limit int
}

func newStore(limit int) *store {
	return &store{m: map[string]query{}, limit: limit}
}

func (s *store) put(q query) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	key := strconv.FormatUint(s.next, 36)
	s.m[key] = q
	s.order = append(s.order, key)
	if len(s.order) > s.limit {
		delete(s.m, s.order[0])
		s.order = s.order[1:]
	}
	return key
}

func (s *store) get(key string) (query, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, ok := s.m[key]
	return q, ok
}
