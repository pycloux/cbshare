package server

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/pycloux/cbshare/internal/config"
	"github.com/pycloux/cbshare/internal/store"
)

func TestSetupFlow(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.client()

	r := admin.expect(admin.do("GET", "/api/state", nil), 200)
	if r.body["setupRequired"] != true || r.body["authenticated"] != false {
		t.Fatalf("state: %v", r.body)
	}

	r = admin.expect(admin.do("POST", "/api/setup/pin", map[string]string{"pin": "WRONG1"}), 401)
	if r.body["remaining"] != float64(4) {
		t.Fatalf("remaining attempts: %v", r.body)
	}
	msgs := h.messages()
	if len(msgs) != 1 || msgs[0].Kind != store.KindSystem || !strings.Contains(msgs[0].Body, "127.0.0.1") {
		t.Fatalf("expected a system message with the IP: %+v", msgs)
	}

	pin := strings.ToLower(h.srv.SetupPIN()) // PINs are case-insensitive
	r = admin.expect(admin.do("POST", "/api/setup/pin", map[string]string{"pin": pin}), 200)
	secret, _ := r.body["secret"].(string)
	if secret == "" || !strings.HasPrefix(r.body["qr"].(string), "data:image/png;base64,") {
		t.Fatalf("enrollment: %v", r.body)
	}

	other := h.client()
	other.expect(other.do("POST", "/api/setup/confirm", nil), 403)
	other.expect(other.do("POST", "/api/login", map[string]string{"username": "admin", "code": "123456"}), 409)

	r = admin.expect(admin.do("POST", "/api/setup/confirm", nil), 200)
	if r.body["username"] != "admin" {
		t.Fatalf("confirm: %v", r.body)
	}
	if h.srv.SetupPIN() != "" {
		t.Fatal("PIN must be cleared after setup")
	}
	u, err := h.st.AdminUser(context.Background())
	if err != nil || u.Name != "admin" || u.Secret != secret || u.Status != store.StatusActive {
		t.Fatalf("admin account: %+v, %v", u, err)
	}
	admin.expect(admin.do("POST", "/api/setup/pin", map[string]string{"pin": pin}), 409)

	r = admin.expect(admin.do("GET", "/api/state", nil), 200)
	if r.body["setupRequired"] != false || r.body["authenticated"] != false {
		t.Fatalf("after setup the admin signs in like everyone: %v", r.body)
	}
}

// The admin QR code shown at setup must be confirmed within 15 minutes;
// entering the PIN again shows a new one.
func TestSetupConfirmationExpires(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.client()
	pin := h.srv.SetupPIN()
	first := admin.expect(admin.do("POST", "/api/setup/pin", map[string]string{"pin": pin}), 200)

	h.advance(enrollmentValid + time.Second)
	r := admin.expect(admin.do("POST", "/api/setup/confirm", nil), 403)
	if r.body["code"] != "setup_expired" {
		t.Fatalf("got %v", r.body)
	}
	if h.srv.SetupPIN() != pin {
		t.Fatal("an expired confirmation must leave setup open with the same PIN")
	}

	again := admin.expect(admin.do("POST", "/api/setup/pin", map[string]string{"pin": pin}), 200)
	if again.body["secret"] == first.body["secret"] {
		t.Fatal("entering the PIN again must show a new secret")
	}
	h.advance(enrollmentValid - time.Second) // still inside the window
	admin.expect(admin.do("POST", "/api/setup/confirm", nil), 200)
}

func TestLoginAndLogout(t *testing.T) {
	h := newHarness(t, nil)
	secret := h.setup()
	c := h.client()

	h.tryLogin(c, "admin", "000000")
	r := c.expect(h.tryLogin(c, "admin", h.code(secret)), 200)
	if r.body["username"] != "admin" {
		t.Fatalf("login: %v", r.body)
	}
	r = c.expect(c.do("GET", "/api/state", nil), 200)
	if r.body["authenticated"] != true || r.body["username"] != "admin" || r.body["isAdminUser"] != true ||
		r.body["admin"] != false || r.body["userId"] == nil || r.body["maxUsers"] != float64(10) {
		t.Fatalf("state: %v", r.body)
	}
	// The username is matched whatever its case and spacing.
	c2 := h.client()
	c2.expect(h.tryLogin(c2, "  ADMIN ", h.code(secret)), 200)

	c.expect(c.do("POST", "/api/logout", nil), 200)
	c.expect(c.do("GET", "/api/messages", nil), 401)
	// The username endpoint is gone.
	c2.expect(c2.do("POST", "/api/username", map[string]string{"username": "x"}), 405)
}

// Each user's code works only with their own name, and every failure looks
// the same, so that the answer does not reveal which names exist.
func TestCodesArePersonal(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.MaxFailedAttempts = 100 })
	h.setup()
	alice := h.addUser("alice")
	bob := h.addUser("bob")
	c := h.client()

	wrongUser := c.expect(h.tryLogin(c, "bob", h.code(alice)), 401)
	unknown := c.expect(h.tryLogin(c, "nobody", h.code(alice)), 401)
	wrongCode := c.expect(h.tryLogin(c, "alice", "000000"), 401)
	for _, r := range []result{unknown, wrongCode} {
		if r.body["error"] != wrongUser.body["error"] || r.body["code"] != wrongUser.body["code"] {
			t.Fatalf("failures differ: %v vs %v", r.body, wrongUser.body)
		}
	}
	msgs := h.messages()
	if last := msgs[len(msgs)-1].Body; strings.Contains(last, "nobody") || !strings.Contains(last, "wrong username or code") {
		t.Fatalf("chat alert must not name the account: %q", last)
	}
	c.expect(h.tryLogin(c, "bob", h.code(bob)), 200)
}

// A code can be used once: someone who sees it typed cannot reuse it.
func TestReplayedCodeRefused(t *testing.T) {
	h := newHarness(t, nil)
	h.setup()
	secret := h.addUser("alice")
	code := h.code(secret)
	a, b := h.client(), h.client()
	a.expect(h.tryLogin(a, "alice", code), 200)
	b.expect(h.tryLogin(b, "alice", code), 401)
	b.expect(h.tryLogin(b, "alice", h.code(secret)), 200)
}

func TestLockout(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.MaxFailedAttempts = 3 })
	secret := h.setup()
	c := h.client()
	before := len(h.messages())

	c.expect(h.tryLogin(c, "admin", "111111"), 401)
	c.expect(h.tryLogin(c, "nobody", "222222"), 401)
	r := c.expect(h.tryLogin(c, "admin", "333333"), 429)
	if r.body["code"] != "locked" || r.body["retryAfter"] != float64(300) {
		t.Fatalf("lockout: %v", r.body)
	}
	c.expect(h.tryLogin(c, "admin", h.code(secret)), 429)

	msgs := h.messages()
	if len(msgs)-before != 3 || !strings.Contains(msgs[len(msgs)-1].Body, "locked out for 5 minutes") {
		t.Fatalf("system messages: %+v", msgs[before:])
	}
}

func TestSecretSurvivesRestart(t *testing.T) {
	h := newHarness(t, nil)
	secret := h.setup()

	srv2, err := New(h.srv.cfg, h.st, log.New(io.Discard, "", 0), "test")
	if err != nil {
		t.Fatal(err)
	}
	if srv2.SetupPIN() != "" || !srv2.setupDone() {
		t.Fatal("a restarted server must reuse the admin account and not ask for setup")
	}
	if u, err := h.st.AdminUser(t.Context()); err != nil || u.Secret != secret {
		t.Fatalf("admin account after restart: %+v, %v", u, err)
	}
}

func TestSessionExpiry(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.SessionTimeout = 50 * time.Millisecond })
	h.setup()
	c := h.login("alice")
	time.Sleep(80 * time.Millisecond)
	r := c.expect(c.do("GET", "/api/messages", nil), 401)
	if r.body["code"] != "unauthenticated" {
		t.Fatalf("got %v", r.body)
	}
}

func TestHumanDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		5 * time.Minute:  "5 minutes",
		time.Minute:      "1 minute",
		90 * time.Second: "2 minutes",
		30 * time.Second: "30 seconds",
		time.Hour:        "1 hour",
	} {
		if got := humanDuration(d); got != want {
			t.Errorf("humanDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
