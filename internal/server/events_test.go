package server

import (
	"bufio"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pycloux/cpsync/internal/config"
)

type sseEvent struct{ name, data string }

// openStream opens the event stream and returns a channel of events.
func (c *client) openStream(t *testing.T) (<-chan sseEvent, func()) {
	t.Helper()
	req, _ := http.NewRequest("GET", c.h.ts.URL+"/api/events", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("event stream: HTTP %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	ch := make(chan sseEvent, 16)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(resp.Body)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			case line == "" && ev.name != "":
				ch <- ev
				ev = sseEvent{}
			}
		}
	}()
	return ch, func() { resp.Body.Close() }
}

func next(t *testing.T, ch <-chan sseEvent) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("stream closed")
		}
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("no event within 3s")
	}
	return sseEvent{}
}

func TestEventStream(t *testing.T) {
	h := newHarness(t, nil)
	h.setup()
	alice := h.login("alice")
	bob := h.login("bob")

	events, closeStream := bob.openStream(t)
	defer closeStream()
	first := next(t, events)
	if first.name != "rev" || first.data == "" {
		t.Fatalf("first event: %+v", first)
	}

	alice.expect(alice.do("POST", "/api/messages", map[string]string{"text": "hi"}), 201)
	ev := next(t, events)
	if ev.name != "rev" || ev.data == first.data {
		t.Fatalf("expected a new revision, got %+v", ev)
	}

	bob.expect(bob.do("POST", "/api/logout", nil), 200)
	if ev := next(t, events); ev.name != "logout" {
		t.Fatalf("expected logout, got %+v", ev)
	}
}

func TestEventStreamRequiresSession(t *testing.T) {
	h := newHarness(t, nil)
	h.setup()
	anon := h.client()
	anon.expect(anon.do("GET", "/api/events", nil), 401)
}

func TestPollingModeDisablesEvents(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Mode = config.ModePolling })
	h.setup()
	c := h.login("alice")
	r := c.expect(c.do("GET", "/api/events", nil), 404)
	if r.body["code"] != "sse_disabled" {
		t.Fatalf("got %v", r.body)
	}
	r = c.expect(c.do("GET", "/api/state", nil), 200)
	if r.body["mode"] != "polling" || r.body["pollIntervalMs"] != float64(3000) {
		t.Fatalf("state: %v", r.body)
	}
}

func TestHubDoesNotBlock(t *testing.T) {
	hb := newHub()
	ch := hb.subscribe()
	for i := 0; i < 10; i++ {
		hb.notify() // nobody reads: must not block
	}
	<-ch
	hb.unsubscribe(ch)
	if hb.count() != 0 {
		t.Fatal("unsubscribe failed")
	}
}
