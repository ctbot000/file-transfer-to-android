package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"
)

// audience selects which pages an event is for.
type audience int

const (
	desktopPages audience = 1 << iota
	phonePages
)

// Event names sent over the server-sent event streams.
const (
	eventState     = "state"     // desktop: the full desktop state, as JSON
	eventTransfers = "transfers" // desktop: transfer progress, as JSON
	eventChanged   = "changed"   // phone: the shared items changed; refetch
)

// hub fans events out to the open event streams. Events are coalesced per
// subscriber: publishing never blocks, and a slow page that misses several
// "state" events gets one, rendered from the state at the time it is sent.
type hub struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
}

type subscriber struct {
	aud     audience
	wake    chan struct{}
	mu      sync.Mutex
	pending map[string]bool
}

func newHub() *hub {
	return &hub{subs: make(map[*subscriber]struct{})}
}

func (h *hub) subscribe(aud audience) *subscriber {
	s := &subscriber{aud: aud, wake: make(chan struct{}, 1), pending: make(map[string]bool)}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

func (h *hub) unsubscribe(s *subscriber) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
}

func (h *hub) publish(aud audience, event string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		if s.aud&aud == 0 {
			continue
		}
		s.mu.Lock()
		s.pending[event] = true
		s.mu.Unlock()
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

// take returns the pending events in a stable order and clears them.
func (s *subscriber) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	events := make([]string, 0, len(s.pending))
	for e := range s.pending {
		events = append(events, e)
	}
	clear(s.pending)
	slices.Sort(events)
	return events
}

const keepAliveInterval = 25 * time.Second

// stream serves an event stream until the client goes away or the server
// closes. initial events are sent right away; payload renders an event's
// data, or returns nil for an event without data.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, sub *subscriber, initial []string, payload func(event string) any) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	// Browsers reconnect by themselves; two seconds keeps a restarted app
	// from looking dead for long.
	fmt.Fprint(w, "retry: 2000\n\n")

	send := func(events []string) error {
		for _, e := range events {
			data := "{}"
			if v := payload(e); v != nil {
				b, err := json.Marshal(v)
				if err != nil {
					return err
				}
				data = string(b)
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e, data); err != nil {
				return err
			}
		}
		return rc.Flush()
	}
	if err := send(initial); err != nil {
		return
	}

	keepAlive := time.NewTicker(keepAliveInterval)
	defer keepAlive.Stop()
	for {
		var err error
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case <-keepAlive.C:
			if _, err = fmt.Fprint(w, ": keep-alive\n\n"); err == nil {
				err = rc.Flush()
			}
		case <-sub.wake:
			err = send(sub.take())
		}
		if err != nil {
			return
		}
	}
}
