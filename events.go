package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// comboEvent intentionally stores routing metadata rather than the raw user
// request or image URL. It is a local, in-memory diagnostic trail.
type comboEvent struct {
	ID         string       `json:"id"`
	StartedAt  time.Time    `json:"started_at"`
	FinishedAt time.Time    `json:"finished_at,omitempty"`
	Requested  string       `json:"requested_model"`
	Primary    string       `json:"primary_model"`
	Stream     bool         `json:"stream"`
	ImageCount int          `json:"image_count"`
	Status     string       `json:"status"`
	Error      string       `json:"error,omitempty"`
	Stages     []eventStage `json:"stages"`
}

type eventStage struct {
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	Model      string    `json:"model,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	DurationMS int64     `json:"duration_ms"`
}

type eventStore struct {
	mu     sync.RWMutex
	limit  int
	nextID uint64
	events []*comboEvent
}

func newEventStore(limit int) *eventStore {
	if limit <= 0 {
		limit = 100
	}
	return &eventStore{limit: limit}
}

func (s *eventStore) setLimit(limit int) {
	if s == nil || limit <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.limit = limit
	if len(s.events) > limit {
		s.events = s.events[:limit]
	}
}

func (s *eventStore) begin(requested, primary string, stream bool) *comboEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	e := &comboEvent{
		ID:        fmt.Sprintf("%s-%04d", time.Now().Format("150405.000"), s.nextID),
		StartedAt: time.Now(),
		Requested: requested,
		Primary:   primary,
		Stream:    stream,
		Status:    "进行中",
	}
	s.events = append([]*comboEvent{e}, s.events...)
	if len(s.events) > s.limit {
		s.events = s.events[:s.limit]
	}
	return e
}

func (s *eventStore) stage(e *comboEvent, name, status, model, detail string, started time.Time) {
	if e == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e.Stages = append(e.Stages, eventStage{
		Name:       name,
		Status:     status,
		Model:      strings.TrimSpace(model),
		Detail:     abbreviateEventText(detail, 640),
		StartedAt:  started,
		DurationMS: time.Since(started).Milliseconds(),
	})
}

func (s *eventStore) setImageCount(e *comboEvent, count int) {
	if e == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e.ImageCount = count
}

func (s *eventStore) finish(e *comboEvent, err error) {
	if e == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e.FinishedAt = time.Now()
	if err != nil {
		e.Status = "失败"
		e.Error = abbreviateEventText(err.Error(), 360)
		return
	}
	e.Status = "完成"
}

func (s *eventStore) snapshot() []comboEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]comboEvent, 0, len(s.events))
	for _, e := range s.events {
		if e == nil {
			continue
		}
		copy := *e
		copy.Stages = append([]eventStage(nil), e.Stages...)
		out = append(out, copy)
	}
	return out
}

func abbreviateEventText(value string, max int) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\x00", ""))
	if max <= 0 || len(value) <= max {
		return value
	}
	return value[:max] + "…"
}

func eventStatusRank(value string) int {
	switch value {
	case "完成":
		return 0
	case "进行中":
		return 1
	default:
		return 2
	}
}

func sortEventsForDisplay(events []comboEvent) {
	sort.SliceStable(events, func(i, j int) bool {
		if eventStatusRank(events[i].Status) != eventStatusRank(events[j].Status) {
			return eventStatusRank(events[i].Status) < eventStatusRank(events[j].Status)
		}
		return events[i].StartedAt.After(events[j].StartedAt)
	})
}
