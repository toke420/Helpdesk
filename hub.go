package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Hub is an in-memory publish/subscribe fan-out for Server-Sent Events.
// Topics: "agents" (every signed-in agent), "agent:<id>", "contact:<id>",
// "visitors" (presence changes for every open widget).
type Hub struct {
	mu   sync.Mutex
	subs map[string]map[chan []byte]struct{}
	// onChange is called (outside the lock) when a topic's subscriber count
	// moves between zero and non-zero.
	onChange func(topic string, active bool)
}

type Event struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

func NewHub() *Hub { return &Hub{subs: map[string]map[chan []byte]struct{}{}} }

func (h *Hub) Subscribe(topics ...string) (chan []byte, func()) {
	ch := make(chan []byte, 32)
	var became []string
	h.mu.Lock()
	for _, t := range topics {
		if h.subs[t] == nil {
			h.subs[t] = map[chan []byte]struct{}{}
		}
		if len(h.subs[t]) == 0 {
			became = append(became, t)
		}
		h.subs[t][ch] = struct{}{}
	}
	h.mu.Unlock()
	h.notify(became, true)
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			var gone []string
			h.mu.Lock()
			for _, t := range topics {
				delete(h.subs[t], ch)
				if len(h.subs[t]) == 0 {
					delete(h.subs, t)
					gone = append(gone, t)
				}
			}
			h.mu.Unlock()
			h.notify(gone, false)
		})
	}
}

func (h *Hub) notify(topics []string, active bool) {
	if h.onChange == nil {
		return
	}
	for _, t := range topics {
		h.onChange(t, active)
	}
}

func (h *Hub) Publish(topic string, ev Event) {
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[topic] {
		select {
		case ch <- b:
		default: // slow client: drop; it re-syncs on its next fetch
		}
	}
}

func (h *Hub) Active(topic string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[topic]) > 0
}

func topicContact(id int64) string { return fmt.Sprintf("contact:%d", id) }
func topicAgent(id int64) string   { return fmt.Sprintf("agent:%d", id) }

// serveSSE streams events for the given topics until the client disconnects
// or the server shuts down.
func (a *App) serveSSE(w http.ResponseWriter, r *http.Request, first []Event, topics ...string) {
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ch, unsubscribe := a.hub.Subscribe(topics...)
	defer unsubscribe()

	fmt.Fprint(w, "retry: 3000\n\n")
	for _, ev := range first {
		b, _ := json.Marshal(ev)
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	if err := rc.Flush(); err != nil {
		return
	}
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case b := <-ch:
			if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
				return
			}
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}
