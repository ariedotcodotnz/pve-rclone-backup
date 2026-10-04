// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
)

// Broker fans out events to subscribers. A subscriber that falls behind
// is disconnected rather than silently losing events; clients reconnect
// and re-read current state.
type Broker struct {
	mu     sync.Mutex
	subs   map[chan apiv1.Event]struct{}
	buf    int
	closed bool
	nextID atomic.Uint64
	now    func() time.Time
}

// NewBroker returns a broker whose subscribers buffer up to buf events.
func NewBroker(buf int) *Broker {
	return &Broker{subs: map[chan apiv1.Event]struct{}{}, buf: buf, now: time.Now}
}

// Publish sends an event to every subscriber without blocking.
func (b *Broker) Publish(typ string, data any) {
	ev := apiv1.Event{ID: b.nextID.Add(1), Type: typ, Time: b.now().UTC(), Data: data}
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
			delete(b.subs, ch)
			close(ch)
		}
	}
}

// Subscribe returns a channel of events and a function to unsubscribe. The
// channel is closed when the subscriber is dropped or the broker closes.
func (b *Broker) Subscribe() (<-chan apiv1.Event, func()) {
	ch := make(chan apiv1.Event, b.buf)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		close(ch)
		return ch, func() {}
	}
	b.subs[ch] = struct{}{}
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
	}
}

// Close disconnects all subscribers.
func (b *Broker) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for ch := range b.subs {
		delete(b.subs, ch)
		close(ch)
	}
}

const sseKeepalive = 15 * time.Second

// handleEvents streams events as text/event-stream.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return Errorf(http.StatusInternalServerError, apiv1.CodeInternal, "streaming unsupported")
	}
	events, unsubscribe := s.events.Subscribe()
	defer unsubscribe()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return nil
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return nil
			}
			flusher.Flush()
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			data, err := json.Marshal(ev)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.ID, ev.Type, data); err != nil {
				return nil
			}
			flusher.Flush()
		}
	}
}
