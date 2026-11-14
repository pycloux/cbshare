package server

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pycloux/cpsync/internal/config"
	"github.com/pycloux/cpsync/internal/store"
)

// unlockedAdmin signs in as the admin account and unlocks admin rights.
func unlockedAdmin(t *testing.T, h *harness) *client {
	t.Helper()
	admin := h.login("admin")
	admin.expect(admin.do("POST", "/api/admin/login", map[string]string{"password": "admin-pass"}), 200)
	return admin
}

// createUser creates a user through the API and returns its id and secret.
func createUser(t *testing.T, admin *client, name string) (string, string) {
	t.Helper()
	r := admin.expect(admin.do("POST", "/api/admin/users", map[string]string{"name": name}), 201)
	user := r.body["user"].(map[string]any)
	enr := r.body["enrollment"].(map[string]any)
	if user["status"] != store.StatusPending || enr["secret"] == "" || !strings.HasPrefix(enr["qr"].(string), "data:image/png") {
		t.Fatalf("create %q: %v", name, r.body)
	}
	if _, leaked := user["secret"]; leaked {
		t.Fatal("the user view must not carry the secret")
	}
	return fmt.Sprint(int(user["id"].(float64))), enr["secret"].(string)
}

func TestCreateUserAndFirstSignIn(t *testing.T) {
	h, _ := newAdminHarness(t)
	admin := unlockedAdmin(t, h)
	id, secret := createUser(t, admin, "  Bob  Smith ")

	r := admin.expect(admin.do("GET", "/api/admin/users", nil), 200)
	users := r.body["users"].([]any)
	if len(users) != 2 || r.body["count"] != float64(2) || r.body["max"] != float64(10) {
		t.Fatalf("list: %v", r.body)
	}
	if first := users[0].(map[string]any); first["name"] != "admin" || first["admin"] != true {
		t.Fatalf("the admin comes first: %v", users)
	}
	bob := users[1].(map[string]any)
	if bob["name"] != "Bob Smith" || bob["enrolExpiresAt"] == nil {
		t.Fatalf("bob: %v", bob)
	}

	// The QR code is never available again.
	if strings.Contains(string(r.raw), secret) {
		t.Fatal("the user list leaks the secret")
	}

	c := h.loginWith("bob smith", secret)
	r = c.expect(c.do("GET", "/api/state", nil), 200)
	if r.body["username"] != "Bob Smith" || r.body["isAdminUser"] != false {
		t.Fatalf("state: %v", r.body)
	}
	u, _ := h.st.UserByName(t.Context(), "Bob Smith")
	if u.Status != store.StatusActive || u.ActivatedAt == nil || fmt.Sprint(u.ID) != id {
		t.Fatalf("after first sign-in: %+v", u)
	}
}

func TestCreateUserRules(t *testing.T) {
	h, _ := newAdminHarnessWith(t, func(c *config.Config) { c.MaxUsers = 3 })
	admin := unlockedAdmin(t, h)
	for _, bad := range []string{"", "   ", "Admin", "ADMIN", "cpsync", strings.Repeat("x", 33), "bell\u0007"} {
		r := admin.expect(admin.do("POST", "/api/admin/users", map[string]string{"name": bad}), 400)
		if r.body["code"] != "invalid_username" {
			t.Fatalf("%q: %v", bad, r.body)
		}
	}
	createUser(t, admin, "carol")
	r := admin.expect(admin.do("POST", "/api/admin/users", map[string]string{"name": "CAROL"}), 409)
	if r.body["code"] != "name_taken" {
		t.Fatalf("duplicate: %v", r.body)
	}
	createUser(t, admin, "dave")
	r = admin.expect(admin.do("POST", "/api/admin/users", map[string]string{"name": "erin"}), 409)
	if r.body["code"] != "user_limit" || !strings.Contains(r.body["error"].(string), "3 users") {
		t.Fatalf("limit: %v", r.body)
	}
}

func TestUserEndpointsNeedAdmin(t *testing.T) {
	h, _ := newAdminHarness(t)
	alice := h.login("alice")
	admin := h.login("admin") // not unlocked
	for _, c := range []*client{alice, admin} {
		c.expect(c.do("GET", "/api/admin/users", nil), 403)
		c.expect(c.do("POST", "/api/admin/users", map[string]string{"name": "x"}), 403)
		c.expect(c.do("DELETE", "/api/admin/users/2", nil), 403)
	}
	anon := h.client()
	anon.expect(anon.do("GET", "/api/admin/users", nil), 401)
}

func TestEnrolmentExpires(t *testing.T) {
	h, _ := newAdminHarness(t)
	admin := unlockedAdmin(t, h)
	_, secret := createUser(t, admin, "frank")
	h.advance(24*time.Hour + time.Minute)

	c := h.client()
	c.expect(h.tryLogin(c, "frank", h.code(secret)), 401)

	r := admin.expect(admin.do("GET", "/api/admin/users", nil), 200)
	frank := r.body["users"].([]any)[1].(map[string]any)
	if frank["status"] != store.StatusExpired {
		t.Fatalf("frank: %v", frank)
	}
	if u, _ := h.st.UserByName(t.Context(), "frank"); u.Secret != "" {
		t.Fatal("an expired user's secret must be erased")
	}
	found := false
	for _, m := range h.messages() {
		found = found || strings.Contains(m.Body, "QR code of frank expired")
	}
	if !found {
		t.Fatal("expiry must be announced")
	}
}

func TestRevokeUser(t *testing.T) {
	h, _ := newAdminHarness(t)
	admin := unlockedAdmin(t, h)
	id, secret := createUser(t, admin, "grace")
	grace := h.loginWith("grace", secret)
	grace2 := h.loginWith("grace", secret)
	other := h.login("heidi")
	events, closeStream := grace.openStream(t)
	defer closeStream()
	next(t, events)

	r := admin.expect(admin.do("POST", "/api/admin/users/"+id+"/revoke", nil), 200)
	if r.body["signedOut"] != float64(2) || r.body["user"].(map[string]any)["status"] != store.StatusRevoked {
		t.Fatalf("revoke: %v", r.body)
	}
	for ev := next(t, events); ev.name != "logout"; ev = next(t, events) {
	}
	grace.expect(grace.do("GET", "/api/messages", nil), 401)
	grace2.expect(grace2.do("GET", "/api/messages", nil), 401)
	other.expect(other.do("GET", "/api/messages", nil), 200)

	c := h.client()
	c.expect(h.tryLogin(c, "grace", h.code(secret)), 401)
}

func TestNewQRCodeForUser(t *testing.T) {
	h, _ := newAdminHarness(t)
	admin := unlockedAdmin(t, h)
	id, oldSecret := createUser(t, admin, "ivan")
	ivan := h.loginWith("ivan", oldSecret)

	r := admin.expect(admin.do("POST", "/api/admin/users/"+id+"/qr", nil), 200)
	newSecret := r.body["enrollment"].(map[string]any)["secret"].(string)
	if newSecret == oldSecret || r.body["user"].(map[string]any)["status"] != store.StatusPending {
		t.Fatalf("new QR code: %v", r.body)
	}
	ivan.expect(ivan.do("GET", "/api/messages", nil), 401)
	c := h.client()
	c.expect(h.tryLogin(c, "ivan", h.code(oldSecret)), 401)
	h.loginWith("ivan", newSecret)

	// A revoked user gets access back with a new QR code.
	admin.expect(admin.do("POST", "/api/admin/users/"+id+"/revoke", nil), 200)
	r = admin.expect(admin.do("POST", "/api/admin/users/"+id+"/qr", nil), 200)
	h.loginWith("ivan", r.body["enrollment"].(map[string]any)["secret"].(string))
}

func TestAdminAccountProtected(t *testing.T) {
	h, _ := newAdminHarness(t)
	admin := unlockedAdmin(t, h)
	u, _ := h.st.AdminUser(t.Context())
	base := fmt.Sprintf("/api/admin/users/%d", u.ID)
	for _, req := range [][2]string{{"POST", base + "/revoke"}, {"POST", base + "/qr"}, {"DELETE", base}} {
		r := admin.expect(admin.do(req[0], req[1], nil), 403)
		if r.body["code"] != "admin_account" {
			t.Fatalf("%v: %v", req, r.body)
		}
	}
	admin.expect(admin.do("DELETE", "/api/admin/users/999", nil), 404)
	admin.expect(admin.do("DELETE", "/api/admin/users/abc", nil), 404)
}

// Deleting a user keeps their messages, marked as from a deleted user; a
// new user with the same name does not get to change them.
func TestDeleteUserKeepsMessages(t *testing.T) {
	h, _ := newAdminHarness(t)
	admin := unlockedAdmin(t, h)
	id, secret := createUser(t, admin, "judy")
	judy := h.loginWith("judy", secret)
	r := judy.expect(judy.do("POST", "/api/messages", map[string]string{"text": "hello"}), 201)
	msgPath := fmt.Sprintf("/api/messages/%d", int(r.body["id"].(float64)))

	r = admin.expect(admin.do("DELETE", "/api/admin/users/"+id, nil), 200)
	if r.body["signedOut"] != float64(1) {
		t.Fatalf("delete: %v", r.body)
	}
	judy.expect(judy.do("GET", "/api/messages", nil), 401)

	r = admin.expect(admin.do("GET", "/api/messages", nil), 200)
	var kept map[string]any
	for _, m := range messageList(r) {
		if m["body"] == "hello" {
			kept = m
		}
	}
	if kept == nil || kept["author"] != "judy" || kept["authorDeleted"] != true || kept["userId"] != nil {
		t.Fatalf("kept message: %v", kept)
	}

	_, secret2 := createUser(t, admin, "Judy")
	judy2 := h.loginWith("judy", secret2)
	judy2.expect(judy2.do("PUT", msgPath, map[string]string{"text": "mine now"}), 403)
	judy2.expect(judy2.do("DELETE", msgPath, nil), 403)
	// The admin can still moderate it.
	admin.expect(admin.do("DELETE", msgPath, nil), 200)
}

func TestMaxUsersCountsTheAdmin(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.MaxUsers = 1 })
	h.setup()
	if n, _ := h.st.CountUsers(t.Context()); n != 1 {
		t.Fatalf("users after setup: %d", n)
	}
}
