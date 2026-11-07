// Package alerts implements sustained thresholds, hysteresis and a durable outbox.
package alerts

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

type Observation struct {
	Key, Title, SourceID string
	Bad, Clear           bool
}
type Incident struct {
	Key      string    `json:"key"`
	Title    string    `json:"title"`
	SourceID string    `json:"source_id,omitempty"`
	Since    time.Time `json:"since"`
	LastSeen time.Time `json:"last_seen"`
	Active   bool      `json:"active"`
}
type Event struct {
	Time      time.Time `json:"time"`
	Title     string    `json:"title"`
	SourceID  string    `json:"source_id,omitempty"`
	Recovered bool      `json:"recovered"`
}
type Delivery struct {
	ID     uint64 `json:"id"`
	ChatID int64  `json:"chat_id"`
	Event  Event  `json:"event"`
}
type state struct {
	Incidents map[string]Incident  `json:"incidents"`
	Muted     map[string]time.Time `json:"muted"`
	History   []Event              `json:"history"`
	Pending   []Delivery           `json:"pending"`
	Next      uint64               `json:"next"`
}
type Engine struct {
	mu           sync.Mutex
	data         state
	path         string
	hold, maxGap time.Duration
	admins       []int64
}

func Open(path string, hold, interval time.Duration, admins []int64) (*Engine, error) {
	e := &Engine{path: path, hold: hold, maxGap: 3 * interval, admins: append([]int64(nil), admins...)}
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err == nil && len(b) == 0 {
		return nil, fmt.Errorf("alert state: empty file %s", path)
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &e.data); err != nil {
			return nil, fmt.Errorf("alert state: %w", err)
		}
	}
	if e.data.Incidents == nil {
		e.data.Incidents = map[string]Incident{}
	}
	if e.data.Muted == nil {
		e.data.Muted = map[string]time.Time{}
	}
	keep := e.data.Pending[:0]
	for _, d := range e.data.Pending {
		for _, id := range e.admins {
			if id == d.ChatID {
				keep = append(keep, d)
				break
			}
		}
	}
	e.data.Pending = keep
	// Fail at startup if persistence is unavailable, rather than silently losing mutes.
	if err := e.save(); err != nil {
		return nil, err
	}
	return e, nil
}

// Prune removes incidents whose rules were deliberately removed from the config.
// Missing measurements of rules still in the config must never use this method.
func (e *Engine) Prune(keys map[string]bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.update(func() {
		for key := range e.data.Incidents {
			if !keys[key] {
				delete(e.data.Incidents, key)
			}
		}
	})
}

// update rolls back memory if the atomic disk write fails.
func (e *Engine) update(fn func()) error {
	before, _ := json.Marshal(e.data)
	fn()
	var err error
	if len(e.data.Pending) > 5000 {
		err = fmt.Errorf("alert outbox full (5000), waiting for delivery")
	} else {
		err = e.save()
	}
	if err != nil {
		e.data = state{}
		_ = json.Unmarshal(before, &e.data)
		return err
	}
	return nil
}

func (e *Engine) Evaluate(now time.Time, observations []Observation) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.update(func() {
		seen := map[string]bool{}
		for _, o := range observations {
			seen[o.Key] = true
			s := e.data.Incidents[o.Key]
			if !s.Active && (s.LastSeen.IsZero() || now.Sub(s.LastSeen) > e.maxGap) {
				s.Since = time.Time{}
			}
			s.Key, s.Title, s.SourceID, s.LastSeen = o.Key, o.Title, o.SourceID, now
			if s.Active {
				if o.Clear {
					e.event(now, s, true)
					delete(e.data.Incidents, o.Key)
					continue
				}
			} else if o.Bad {
				if s.Since.IsZero() {
					s.Since = now
				}
				if now.Sub(s.Since) >= e.hold {
					s.Active = true
					e.event(now, s, false)
				}
			} else {
				delete(e.data.Incidents, o.Key)
				continue
			}
			e.data.Incidents[o.Key] = s
		}
		// Missing/failed samples break a pending threshold, never claim recovery.
		for key, s := range e.data.Incidents {
			if !seen[key] && !s.Active {
				delete(e.data.Incidents, key)
			}
		}
	})
}

func (e *Engine) event(now time.Time, s Incident, recovered bool) {
	ev := Event{Time: now, Title: s.Title, SourceID: s.SourceID, Recovered: recovered}
	e.data.History = append(e.data.History, ev)
	if len(e.data.History) > 50 {
		e.data.History = e.data.History[len(e.data.History)-50:]
	}
	seen := map[int64]bool{}
	for _, id := range e.admins {
		if seen[id] || e.data.Muted[strconv.FormatInt(id, 10)].After(now) {
			continue
		}
		seen[id] = true
		e.data.Next++
		e.data.Pending = append(e.data.Pending, Delivery{ID: e.data.Next, ChatID: id, Event: ev})
	}
}

func (e *Engine) Mute(chatID int64, until time.Time) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.update(func() {
		e.data.Muted[strconv.FormatInt(chatID, 10)] = until
		pending := e.data.Pending[:0]
		for _, d := range e.data.Pending {
			if d.ChatID != chatID {
				pending = append(pending, d)
			}
		}
		e.data.Pending = pending
	})
}

func (e *Engine) View(chatID int64) ([]Incident, []Event, time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var active []Incident
	for _, s := range e.data.Incidents {
		if s.Active {
			active = append(active, s)
		}
	}
	sort.Slice(active, func(i, j int) bool { return active[i].Key < active[j].Key })
	return active, append([]Event(nil), e.data.History...), e.data.Muted[strconv.FormatInt(chatID, 10)]
}

func (e *Engine) Pending(now time.Time) []Delivery {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []Delivery
	for _, d := range e.data.Pending {
		allowed := false
		for _, id := range e.admins {
			if d.ChatID == id {
				allowed = true
				break
			}
		}
		if allowed && !e.data.Muted[strconv.FormatInt(d.ChatID, 10)].After(now) {
			out = append(out, d)
		}
	}
	return out
}

func (e *Engine) Ack(id uint64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.update(func() {
		for i, d := range e.data.Pending {
			if d.ID == id {
				e.data.Pending = append(e.data.Pending[:i], e.data.Pending[i+1:]...)
				break
			}
		}
	})
}

func (e *Engine) save() error {
	if err := os.MkdirAll(filepath.Dir(e.path), 0o700); err != nil {
		return fmt.Errorf("alert state directory: %w", err)
	}
	b, err := json.Marshal(e.data)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(e.path), ".alerts-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, e.path)
}
