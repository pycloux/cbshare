package server

// Live updates.
//
// The events carry no chat data, only the current revision number. A
// browser that sees a revision different from its own fetches the list
// with GET /api/messages. Keeping data out of the stream means there is a
// single code path for loading messages, shared with polling mode, and
// that a missed or merged notification can never leave a browser with a
// wrong view: the next fetch always returns the full, current list.

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/pycloux/cbshare/internal/config"
)

// hub notifies event-stream subscribers that the chat changed. Each open
// stream owns a channel with a buffer of one: a notification is either
// pending or not, which is all a subscriber needs to know.
type hub struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

// newHub returns a hub without subscribers.
func newHub() *hub { return &hub{subs: map[chan struct{}]struct{}{}} }

// subscribe registers a new stream and returns its notification channel.
func (h *hub) subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

// unsubscribe removes a stream; call it when the stream ends.
func (h *hub) unsubscribe(ch chan struct{}) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

// notify wakes every subscriber without blocking; a pending notification
// already covers a new one, since subscribers fetch the latest revision.
func (h *hub) notify() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// count returns the number of open streams (used in tests).
func (h *hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// heartbeat keeps idle event streams open through proxies and NAT.
const heartbeat = 20 * time.Second

// GET /api/events — Server-Sent Events stream. Each "rev" event carries the
// current chat revision; a "logout" event means the session has ended.
// Only available in SSE mode; in polling mode browsers poll /api/messages.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Mode != config.ModeSSE {
		writeError(w, http.StatusNotFound, "sse_disabled", "Live updates are off; this server uses polling.")
		return
	}
	// The ResponseController flushes each event to the network right away
	// instead of letting it sit in a buffer.
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // ask proxies such as nginx not to buffer
	w.WriteHeader(http.StatusOK)

	ss := sessionFrom(r)
	sub := s.hub.subscribe()
	defer s.hub.unsubscribe(sub)

	// send writes one event and flushes it; false means the browser is gone.
	send := func(format string, args ...any) bool {
		if _, err := fmt.Fprintf(w, format, args...); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	// "retry" tells EventSource to reconnect after 3 s if the connection
	// drops. The first event gives the browser the current revision.
	if !send("retry: 3000\nevent: rev\ndata: %d\n\n", s.revision.Load()) {
		return
	}

	tick := time.NewTicker(heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ss.Done:
			// The session ended (sign-out, expiry, secret rotation): tell the
			// page to show the sign-in screen.
			send("event: logout\ndata: {}\n\n")
			return
		case <-sub:
			if !send("event: rev\ndata: %d\n\n", s.revision.Load()) {
				return
			}
		case <-tick.C:
			// A comment line: ignored by EventSource, but keeps the
			// connection from being closed as idle.
			if !send(": ping\n\n") {
				return
			}
		}
	}
}
