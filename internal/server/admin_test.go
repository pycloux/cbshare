package server

import (
	"bytes"
	"fmt"
	"log"
	"strings"
	"testing"

	"github.com/pycloux/cbshare/internal/crypto"
	"github.com/pycloux/cbshare/internal/store"
)

// newAdminHarness stores an admin password as the first start does.
func newAdminHarness(t *testing.T) (*harness, string) {
	h := newHarness(t, nil)
	hash, err := crypto.HashPassword("admin-pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetSetting(t.Context(), store.SettingAdminHash, []byte(hash)); err != nil {
		t.Fatal(err)
	}
	return h, h.setup()
}

func TestAdminModeration(t *testing.T) {
	h, _ := newAdminHarness(t)
	alice := h.login("alice")
	admin := h.login("admin")

	r := alice.expect(alice.do("POST", "/api/messages", map[string]string{"text": "oops: password123"}), 201)
	path := fmt.Sprintf("/api/messages/%d", int(r.body["id"].(float64)))

	admin.expect(admin.do("DELETE", path, nil), 403)
	admin.expect(admin.do("POST", "/api/admin/rotate", nil), 403)

	before := len(h.messages())
	admin.expect(admin.do("POST", "/api/admin/login", map[string]string{"password": "wrong"}), 401)
	msgs := h.messages()
	if len(msgs) != before+1 || !strings.Contains(msgs[len(msgs)-1].Body, "wrong admin password") {
		t.Fatalf("failed admin login must be reported in the chat: %+v", msgs)
	}

	admin.expect(admin.do("POST", "/api/admin/login", map[string]string{"password": "admin-pass"}), 200)
	r = admin.expect(admin.do("GET", "/api/state", nil), 200)
	if r.body["admin"] != true {
		t.Fatal("state must report admin")
	}

	admin.expect(admin.do("PUT", path, map[string]string{"text": "[redacted]"}), 200)
	// System messages: admin may delete, nobody may edit.
	sys := msgs[len(msgs)-1]
	sysPath := fmt.Sprintf("/api/messages/%d", sys.ID)
	alice.expect(alice.do("DELETE", sysPath, nil), 403)
	admin.expect(admin.do("PUT", sysPath, map[string]string{"text": "x"}), 403)
	admin.expect(admin.do("DELETE", sysPath, nil), 200)
	admin.expect(admin.do("DELETE", path, nil), 200)

	admin.expect(admin.do("POST", "/api/admin/logout", nil), 200)
	r = alice.expect(alice.do("POST", "/api/messages", map[string]string{"text": "again"}), 201)
	admin.expect(admin.do("DELETE", fmt.Sprintf("/api/messages/%d", int(r.body["id"].(float64))), nil), 403)
}

// The admin replaces their own code in two steps; only the admin's
// sessions end, and other users are not affected.
func TestRotateAdminSecret(t *testing.T) {
	h, secret := newAdminHarness(t)
	alice := h.login("alice")
	admin := h.login("admin")
	admin.expect(admin.do("POST", "/api/admin/login", map[string]string{"password": "admin-pass"}), 200)

	// Cancelling keeps the current secret.
	admin.expect(admin.do("POST", "/api/admin/rotate", nil), 200)
	admin.expect(admin.do("POST", "/api/admin/rotate/cancel", nil), 200)
	admin.expect(admin.do("POST", "/api/admin/rotate/confirm", nil), 409)

	events, closeStream := admin.openStream(t)
	defer closeStream()
	next(t, events)

	r := admin.expect(admin.do("POST", "/api/admin/rotate", nil), 200)
	newSecret := r.body["secret"].(string)
	if newSecret == secret || !strings.HasPrefix(r.body["qr"].(string), "data:image/png") ||
		!strings.Contains(r.body["uri"].(string), "admin%40") && !strings.Contains(r.body["uri"].(string), "admin@") {
		t.Fatalf("expected a new secret and QR code labelled admin: %v", r.body["uri"])
	}
	r = admin.expect(admin.do("POST", "/api/admin/rotate/confirm", nil), 200)
	if r.body["signedOut"] != float64(1) {
		t.Fatalf("signedOut: %v", r.body)
	}

	// The admin is disconnected; alice is not.
	for ev := next(t, events); ev.name != "logout"; ev = next(t, events) {
	}
	admin.expect(admin.do("GET", "/api/messages", nil), 401)
	alice.expect(alice.do("GET", "/api/messages", nil), 200)

	// The old code no longer works, the new one does, and it is persisted.
	c := h.client()
	c.expect(h.tryLogin(c, "admin", h.code(secret)), 401)
	h.loginWith("admin", newSecret)
	if u, _ := h.st.AdminUser(t.Context()); u.Secret != newSecret {
		t.Fatal("new secret must be stored")
	}
	found := false
	for _, m := range h.messages() {
		found = found || strings.Contains(m.Body, "replaced their own authenticator code")
	}
	if !found {
		t.Fatal("rotation must be announced")
	}
}

// Only the admin account may unlock admin tools; others are refused
// without the attempt counting toward a lockout.
func TestAdminUnlockOnlyForAdminAccount(t *testing.T) {
	h, _ := newAdminHarness(t)
	alice := h.login("alice")
	before := len(h.messages())
	for range 10 {
		r := alice.expect(alice.do("POST", "/api/admin/login", map[string]string{"password": "admin-pass"}), 403)
		if r.body["code"] != "admin_only" {
			t.Fatalf("got %v", r.body)
		}
	}
	if len(h.messages()) != before {
		t.Fatal("refused unlocks must not be reported as failed attempts")
	}
	if r := alice.expect(alice.do("GET", "/api/state", nil), 200); r.body["isAdminUser"] != false {
		t.Fatalf("state: %v", r.body)
	}
	admin := h.login("admin")
	admin.expect(admin.do("POST", "/api/admin/login", map[string]string{"password": "admin-pass"}), 200)
}

func TestRotationBelongsToItsSession(t *testing.T) {
	h, _ := newAdminHarness(t)
	a := h.login("admin")
	b := h.login("admin")
	for _, c := range []*client{a, b} {
		c.expect(c.do("POST", "/api/admin/login", map[string]string{"password": "admin-pass"}), 200)
	}
	a.expect(a.do("POST", "/api/admin/rotate", nil), 200)
	b.expect(b.do("POST", "/api/admin/rotate/confirm", nil), 409)
}

func TestAdminLoginWithoutHashLogsReason(t *testing.T) {
	h := newHarness(t, nil) // no admin hash stored, as after an incomplete first start
	var logs bytes.Buffer
	h.srv.log = log.New(&logs, "", 0)
	h.setup()
	c := h.login("admin")

	r := c.expect(c.do("POST", "/api/admin/login", map[string]string{"password": "anything"}), 500)
	if r.body["code"] != "internal" {
		t.Fatalf("got %v", r.body)
	}
	if !strings.Contains(logs.String(), "admin password hash missing") || strings.Contains(logs.String(), "<nil>") {
		t.Fatalf("console log should explain the error, got:\n%s", logs.String())
	}
}
