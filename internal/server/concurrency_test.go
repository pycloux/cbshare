package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pycloux/cbshare/internal/config"
	"github.com/pycloux/cbshare/internal/crypto"
	"github.com/pycloux/cbshare/internal/store"
)

// burst sends n copies of the same request at once from client c's
// session and returns how many responses had each status code (0: no
// response within a minute). The long timeout keeps the test independent
// of machine speed (Argon2 is slow under the race detector).
func burst(t *testing.T, c *client, n int, method, path string, body any) map[int]int {
	t.Helper()
	var (
		mu     sync.Mutex
		counts = map[int]int{}
		wg     sync.WaitGroup
		start  = make(chan struct{})
		hc     = &http.Client{Transport: c.http.Transport, Jar: c.http.Jar, Timeout: time.Minute}
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			status := 0 // no response (timeout or error)
			b, _ := json.Marshal(body)
			req, _ := http.NewRequest(method, c.h.ts.URL+path, bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", c.h.ts.URL)
			req.Header.Set("X-CSRF-Token", c.csrf)
			if resp, err := hc.Do(req); err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				status = resp.StatusCode
			}
			mu.Lock()
			counts[status]++
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()
	return counts
}

// Parallel guesses must not get around the lockout: at most
// max_failed_attempts guesses are evaluated, the others are refused, and
// every request is answered (none hangs behind the others).
func TestParallelAdminPasswordGuessesRespectLockout(t *testing.T) {
	h, _ := newAdminHarness(t)
	c := h.login("admin")

	counts := burst(t, c, 40, "POST", "/api/admin/login", map[string]string{"password": "wrong"})

	if counts[http.StatusUnauthorized] > 5 {
		t.Errorf("%d wrong passwords were evaluated, want at most 5 (responses: %v)", counts[http.StatusUnauthorized], counts)
	}
	if counts[http.StatusUnauthorized]+counts[http.StatusTooManyRequests] != 40 {
		t.Errorf("every request should get 401 or 429, got %v", counts)
	}
	// The address is now locked, even for the right password.
	c.expect(c.do("POST", "/api/admin/login", map[string]string{"password": "admin-pass"}), http.StatusTooManyRequests)
}

func TestParallelCodeGuessesRespectLockout(t *testing.T) {
	h, _ := newAdminHarness(t)
	c := h.client()
	counts := burst(t, c, 40, "POST", "/api/login", map[string]string{"code": "000000"})
	if counts[http.StatusUnauthorized] > 5 {
		t.Errorf("%d wrong codes were evaluated, want at most 5 (responses: %v)", counts[http.StatusUnauthorized], counts)
	}
	if counts[http.StatusUnauthorized]+counts[http.StatusTooManyRequests] != 40 {
		t.Errorf("every request should get 401 or 429, got %v", counts)
	}
}

// newAdminHarnessWith is newAdminHarness with a configuration change.
func newAdminHarnessWith(t *testing.T, modify func(*config.Config)) (*harness, string) {
	h := newHarness(t, modify)
	hash, err := crypto.HashPassword("admin-pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetSetting(t.Context(), store.SettingAdminHash, []byte(hash)); err != nil {
		t.Fatal(err)
	}
	return h, h.setup()
}

// Whatever the number of requests, at most maxConcurrentHashes admin
// password checks run at the same time.
func TestAdminPasswordChecksAreLimited(t *testing.T) {
	h, _ := newAdminHarnessWith(t, func(c *config.Config) { c.MaxFailedAttempts = 100 })
	var inFlight, peak atomic.Int32
	h.srv.verifyPassword = func(password, hash string) bool {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		inFlight.Add(-1)
		return false
	}
	c := h.login("admin")
	counts := burst(t, c, 20, "POST", "/api/admin/login", map[string]string{"password": "wrong"})
	if counts[http.StatusUnauthorized] != 20 {
		t.Fatalf("all 20 should be checked (limit 100), got %v", counts)
	}
	if p := peak.Load(); p > maxConcurrentHashes {
		t.Fatalf("%d password checks ran at once, want at most %d", p, maxConcurrentHashes)
	}
}

// When no check slot frees up in time, the request is answered "busy"
// and is not counted as a failed attempt.
func TestAdminPasswordCheckBusy(t *testing.T) {
	h, _ := newAdminHarnessWith(t, func(c *config.Config) { c.MaxFailedAttempts = 100 })
	h.srv.hashWait = 20 * time.Millisecond
	h.srv.verifyPassword = func(password, hash string) bool {
		time.Sleep(300 * time.Millisecond)
		return false
	}
	c := h.login("admin")
	before := len(h.messages())
	counts := burst(t, c, 6, "POST", "/api/admin/login", map[string]string{"password": "wrong"})
	if counts[http.StatusServiceUnavailable] == 0 || counts[http.StatusUnauthorized] == 0 ||
		counts[http.StatusServiceUnavailable]+counts[http.StatusUnauthorized] != 6 {
		t.Fatalf("expected some checked (401) and some busy (503) answers, got %v", counts)
	}
	if alerts := len(h.messages()) - before; alerts != counts[http.StatusUnauthorized] {
		t.Fatalf("%d failure alerts for %d checked passwords: busy answers must not count", alerts, counts[http.StatusUnauthorized])
	}
}
