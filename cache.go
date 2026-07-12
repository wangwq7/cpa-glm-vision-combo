package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type cacheRecord struct {
	Value      string    `json:"value"`
	Kind       string    `json:"kind"`
	Expires    time.Time `json:"expires"`
	LastAccess time.Time `json:"last_access"`
}

type cacheSnapshot struct {
	Version int                    `json:"version"`
	Values  map[string]cacheRecord `json:"values"`
}

type cacheFlight struct {
	done  chan struct{}
	value string
	err   error
}

// memoCache is a process-local L1 cache backed by a small JSON L2 store.
// Writes are coalesced in the background so cache persistence never extends
// the critical path of a visual request.
type memoCache struct {
	mu      sync.Mutex
	values  map[string]cacheRecord
	flights map[string]*cacheFlight
	limit   int
	path    string
	dirty   chan struct{}
	stop    chan struct{}
	done    chan struct{}
}

func newMemoCache(limit int, path string) *memoCache {
	m := &memoCache{
		values:  map[string]cacheRecord{},
		flights: map[string]*cacheFlight{},
		limit:   limit,
		path:    path,
		dirty:   make(chan struct{}, 1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	m.load()
	go m.writer()
	return m
}

func (m *memoCache) compatible(path string) bool {
	return m != nil && m.path == path
}

func (m *memoCache) setLimit(limit int) {
	m.mu.Lock()
	m.limit = limit
	m.pruneLocked(time.Now())
	m.mu.Unlock()
	m.markDirty()
}

func (m *memoCache) get(k string) (string, bool) {
	if m == nil || k == "" {
		return "", false
	}
	now := time.Now()
	m.mu.Lock()
	v, ok := m.values[k]
	if !ok || now.After(v.Expires) {
		delete(m.values, k)
		m.mu.Unlock()
		if ok {
			m.markDirty()
		}
		return "", false
	}
	v.LastAccess = now
	m.values[k] = v
	m.mu.Unlock()
	return v.Value, true
}

func (m *memoCache) set(k, kind, value string, ttl time.Duration) {
	if m == nil || k == "" || value == "" {
		return
	}
	now := time.Now()
	m.mu.Lock()
	m.values[k] = cacheRecord{Value: value, Kind: kind, Expires: now.Add(ttl), LastAccess: now}
	m.pruneLocked(now)
	m.mu.Unlock()
	m.markDirty()
}

func (m *memoCache) do(k string, fn func() (string, error)) (value string, joined bool, err error) {
	if k == "" {
		value, err = fn()
		return value, false, err
	}
	m.mu.Lock()
	if flight := m.flights[k]; flight != nil {
		m.mu.Unlock()
		<-flight.done
		return flight.value, true, flight.err
	}
	flight := &cacheFlight{done: make(chan struct{})}
	m.flights[k] = flight
	m.mu.Unlock()

	flight.value, flight.err = fn()
	m.mu.Lock()
	delete(m.flights, k)
	close(flight.done)
	m.mu.Unlock()
	return flight.value, false, flight.err
}

func (m *memoCache) pruneLocked(now time.Time) {
	for key, record := range m.values {
		if now.After(record.Expires) {
			delete(m.values, key)
		}
	}
	if m.limit <= 0 || len(m.values) <= m.limit {
		return
	}
	type entry struct {
		key  string
		used time.Time
	}
	entries := make([]entry, 0, len(m.values))
	for key, record := range m.values {
		entries = append(entries, entry{key: key, used: record.LastAccess})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].used.Before(entries[j].used) })
	for index := 0; index < len(entries)-m.limit; index++ {
		delete(m.values, entries[index].key)
	}
}

func (m *memoCache) markDirty() {
	if m == nil || m.path == "" {
		return
	}
	select {
	case m.dirty <- struct{}{}:
	default:
	}
}

func (m *memoCache) writer() {
	defer close(m.done)
	var timer *time.Timer
	var timerC <-chan time.Time
	for {
		select {
		case <-m.dirty:
			if timer == nil {
				timer = time.NewTimer(250 * time.Millisecond)
				timerC = timer.C
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(250 * time.Millisecond)
			}
		case <-timerC:
			m.flush()
			timerC = nil
			timer = nil
		case <-m.stop:
			if timer != nil {
				timer.Stop()
			}
			m.flush()
			return
		}
	}
}

func (m *memoCache) load() {
	if m.path == "" {
		return
	}
	raw, err := os.ReadFile(m.path)
	if err != nil {
		return
	}
	var snapshot cacheSnapshot
	if json.Unmarshal(raw, &snapshot) != nil || snapshot.Values == nil {
		return
	}
	m.values = snapshot.Values
	m.pruneLocked(time.Now())
}

func (m *memoCache) flush() {
	if m.path == "" {
		return
	}
	m.mu.Lock()
	m.pruneLocked(time.Now())
	copyValues := make(map[string]cacheRecord, len(m.values))
	for key, value := range m.values {
		copyValues[key] = value
	}
	m.mu.Unlock()
	raw, err := json.Marshal(cacheSnapshot{Version: 1, Values: copyValues})
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Dir(m.path), 0o755) != nil {
		return
	}
	temporary := m.path + ".tmp"
	if os.WriteFile(temporary, raw, 0o600) != nil {
		return
	}
	_ = os.Rename(temporary, m.path)
}

func (m *memoCache) close() {
	if m == nil {
		return
	}
	select {
	case <-m.stop:
		return
	default:
		close(m.stop)
		<-m.done
	}
}
