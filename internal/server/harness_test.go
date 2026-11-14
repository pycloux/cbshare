package server

// Test helpers for the server package.
//
// Tests run a real Server behind httptest's TLS server and talk to it like
// browsers do: each client has its own cookie jar, sends the Origin header
// and, once signed in, the anti-CSRF token. A typical test reads:
//
//	h := newHarness(t, nil)          // fresh encrypted database and server
//	h.setup()                        // first-time setup: the admin account
//	alice := h.login("alice")        // creates the user alice if needed, then signs in
//	alice.expect(alice.do("POST", "/api/messages", body), 201)
//
// The server's clock (srv.now) is the harness clock: every sign-in moves it
// 30 seconds ahead, so that each one uses a code of a new time step and the
// replay guard accepts it. h.advance moves it further, e.g. past the 24-hour
// enrolment window.
//
// The test files mirror the code: auth_test.go, chat_test.go,
// events_test.go, admin_test.go and security_test.go.

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pycloux/cbshare/internal/auth"
	"github.com/pycloux/cbshare/internal/config"
	"github.com/pycloux/cbshare/internal/store"
)

// harness runs a Server behind an httptest TLS server.
type harness struct {
	t   *testing.T
	srv *Server
	ts  *httptest.Server
	st  *store.Store

	mu      sync.Mutex
	clock   time.Time
	secrets map[string]string // TOTP secret of each user, by name
}

func newHarness(t *testing.T, modify func(*config.Config)) *harness {
	t.Helper()
	st := newTestStore(t)
	cfg := config.Default()
	cfg.Bind = "127.0.0.1"
	if modify != nil {
		modify(&cfg)
	}
	srv, err := New(cfg, st, log.New(io.Discard, "", 0), "test")
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, srv: srv, st: st, clock: time.Now(), secrets: map[string]string{}}
	srv.now = h.now
	h.ts = httptest.NewTLSServer(srv.Handler())
	t.Cleanup(func() {
		srv.sessions.EndAll()
		h.ts.Close()
	})
	return h
}

// now is the server's clock in tests.
func (h *harness) now() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.clock
}

// advance moves the server's clock forward.
func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clock = h.clock.Add(d)
}

// code returns a code for secret in a fresh time step, moving the clock
// ahead 30 seconds first so that the replay guard accepts it.
func (h *harness) code(secret string) string {
	h.t.Helper()
	h.advance(30 * time.Second)
	code, err := auth.Code(secret, h.now())
	if err != nil {
		h.t.Fatal(err)
	}
	return code
}

// client is one browser with its own cookies.
type client struct {
	h    *harness
	http *http.Client
	csrf string
}

func (h *harness) client() *client {
	jar, _ := cookiejar.New(nil)
	c := h.ts.Client()
	return &client{h: h, http: &http.Client{Transport: c.Transport, Jar: jar, Timeout: 10 * time.Second}}
}

type result struct {
	status int
	body   map[string]any
	raw    []byte
	header http.Header
}

func (c *client) do(method, path string, body any) result {
	c.h.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.h.ts.URL+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.send(req)
}

func (c *client) send(req *http.Request) result {
	c.h.t.Helper()
	if c.csrf != "" {
		req.Header.Set("X-CSRF-Token", c.csrf)
	}
	req.Header.Set("Origin", c.h.ts.URL)
	resp, err := c.http.Do(req)
	if err != nil {
		c.h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	r := result{status: resp.StatusCode, raw: raw, header: resp.Header}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		json.Unmarshal(raw, &r.body)
	}
	return r
}

func (c *client) expect(r result, status int) result {
	c.h.t.Helper()
	if r.status != status {
		c.h.t.Fatalf("got HTTP %d, want %d: %s", r.status, status, r.raw)
	}
	return r
}

// setup completes the first-time setup and returns the admin account's
// secret.
func (h *harness) setup() string {
	h.t.Helper()
	admin := h.client()
	r := admin.expect(admin.do("POST", "/api/setup/pin", map[string]string{"pin": h.srv.SetupPIN()}), 200)
	secret := r.body["secret"].(string)
	admin.expect(admin.do("POST", "/api/setup/confirm", nil), 200)
	h.setSecret(adminName, secret)
	return secret
}

func (h *harness) setSecret(name, secret string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.secrets[name] = secret
}

func (h *harness) secret(name string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.secrets[name]
}

// addUser creates an active user directly in the store and returns the
// user's secret. Tests of the user-management API go through the API.
func (h *harness) addUser(name string) string {
	h.t.Helper()
	enr, err := auth.NewEnrollment(name)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.st.CreateUser(h.t.Context(), store.NewUser{Name: name, Status: store.StatusActive, Secret: enr.Secret}, 100, h.now()); err != nil {
		h.t.Fatal(err)
	}
	h.setSecret(name, enr.Secret)
	return enr.Secret
}

// login signs a new client in as username, creating the user first if
// the harness does not know it.
func (h *harness) login(username string) *client {
	h.t.Helper()
	secret := h.secret(username)
	if secret == "" {
		secret = h.addUser(username)
	}
	return h.loginWith(username, secret)
}

// loginWith signs a new client in as username with a code of secret.
func (h *harness) loginWith(username, secret string) *client {
	h.t.Helper()
	c := h.client()
	r := c.expect(c.do("POST", "/api/login", map[string]string{"username": username, "code": h.code(secret)}), 200)
	c.csrf = r.body["csrf"].(string)
	return c
}

// tryLogin attempts a sign-in and returns the response.
func (h *harness) tryLogin(c *client, username, code string) result {
	h.t.Helper()
	r := c.do("POST", "/api/login", map[string]string{"username": username, "code": code})
	if r.status == 200 {
		c.csrf = r.body["csrf"].(string)
	}
	return r
}

func (h *harness) messages() []store.Message {
	h.t.Helper()
	msgs, err := h.st.Messages(h.t.Context())
	if err != nil {
		h.t.Fatal(err)
	}
	return msgs
}
