package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func mustCreate(t *testing.T, s *Store, nu NewUser) User {
	t.Helper()
	u, err := s.CreateUser(context.Background(), nu, 10, t0)
	if err != nil {
		t.Fatalf("CreateUser(%q): %v", nu.Name, err)
	}
	return u
}

func TestCreateAndFindUsers(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)

	admin := mustCreate(t, s, NewUser{Name: "admin", Admin: true, Status: StatusActive, Secret: "AAAA"})
	alice := mustCreate(t, s, NewUser{Name: "Alice", Status: StatusPending, Secret: "BBBB", EnrolExpiresAt: t0.Add(24 * time.Hour)})

	got, err := s.UserByName(ctx, "ALICE")
	if err != nil || got.ID != alice.ID || got.Name != "Alice" || got.Secret != "BBBB" || got.Status != StatusPending {
		t.Fatalf("UserByName = %+v, %v", got, err)
	}
	if got.EnrolExpiresAt == nil || !got.EnrolExpiresAt.Equal(t0.Add(24*time.Hour)) {
		t.Fatalf("EnrolExpiresAt = %v", got.EnrolExpiresAt)
	}
	if a, err := s.AdminUser(ctx); err != nil || a.ID != admin.ID || !a.Admin || a.ActivatedAt == nil {
		t.Fatalf("AdminUser = %+v, %v", a, err)
	}
	if _, err := s.UserByName(ctx, "bob"); !errors.Is(err, ErrNoUser) {
		t.Fatalf("unknown user: %v", err)
	}
	if _, err := s.UserByID(ctx, 999); !errors.Is(err, ErrNoUser) {
		t.Fatalf("unknown id: %v", err)
	}

	// Names are unique whatever their letter case.
	if _, err := s.CreateUser(ctx, NewUser{Name: "aLiCe", Status: StatusPending}, 10, t0); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate name: %v", err)
	}
	// Only one admin.
	if _, err := s.CreateUser(ctx, NewUser{Name: "boss", Admin: true, Status: StatusActive}, 10, t0); err == nil {
		t.Fatal("a second admin was accepted")
	}

	users, err := s.ListUsers(ctx)
	if err != nil || len(users) != 2 || !users[0].Admin || users[1].Name != "Alice" {
		t.Fatalf("ListUsers = %+v, %v", users, err)
	}
	if n, _ := s.CountUsers(ctx); n != 2 {
		t.Fatalf("CountUsers = %d", n)
	}
}

func TestUserLimit(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	for _, name := range []string{"admin", "a", "b"} {
		if _, err := s.CreateUser(ctx, NewUser{Name: name, Status: StatusActive}, 3, t0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateUser(ctx, NewUser{Name: "c", Status: StatusActive}, 3, t0); !errors.Is(err, ErrUserLimit) {
		t.Fatalf("over the limit: %v", err)
	}
}

func TestUserLifecycle(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	admin := mustCreate(t, s, NewUser{Name: "admin", Admin: true, Status: StatusActive, Secret: "AAAA"})
	bob := mustCreate(t, s, NewUser{Name: "bob", Status: StatusPending, Secret: "BBBB", EnrolExpiresAt: t0.Add(time.Hour)})

	// First sign-in activates.
	if activated, err := s.RecordLogin(ctx, bob.ID, t0.Add(time.Minute)); err != nil || !activated {
		t.Fatalf("RecordLogin = %v, %v", activated, err)
	}
	if activated, err := s.RecordLogin(ctx, bob.ID, t0.Add(2*time.Minute)); err != nil || activated {
		t.Fatalf("second RecordLogin = %v, %v", activated, err)
	}
	got, _ := s.UserByID(ctx, bob.ID)
	if got.Status != StatusActive || got.EnrolExpiresAt != nil || got.ActivatedAt == nil || !got.LastLoginAt.Equal(t0.Add(2*time.Minute)) {
		t.Fatalf("after sign-in: %+v", got)
	}

	// Revoke erases the secret; the admin cannot be revoked.
	if err := s.RevokeUser(ctx, bob.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.UserByID(ctx, bob.ID); got.Status != StatusRevoked || got.Secret != "" {
		t.Fatalf("after revoke: %+v", got)
	}
	if err := s.RevokeUser(ctx, admin.ID); !errors.Is(err, ErrNoUser) {
		t.Fatalf("revoking the admin: %v", err)
	}

	// A new QR code makes the user pending again.
	exp := t0.Add(3 * time.Hour)
	if err := s.SetUserSecret(ctx, bob.ID, "CCCC", &exp); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.UserByID(ctx, bob.ID); got.Status != StatusPending || got.Secret != "CCCC" || !got.EnrolExpiresAt.Equal(exp) {
		t.Fatalf("after new QR code: %+v", got)
	}

	// The admin replacing their own code stays active.
	if err := s.SetUserSecret(ctx, admin.ID, "DDDD", nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.UserByID(ctx, admin.ID); got.Status != StatusActive || got.Secret != "DDDD" {
		t.Fatalf("admin after new code: %+v", got)
	}
}

func TestExpirePending(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	early := mustCreate(t, s, NewUser{Name: "early", Status: StatusPending, Secret: "A", EnrolExpiresAt: t0.Add(time.Hour)})
	late := mustCreate(t, s, NewUser{Name: "late", Status: StatusPending, Secret: "B", EnrolExpiresAt: t0.Add(48 * time.Hour)})

	expired, err := s.ExpirePending(ctx, t0.Add(2*time.Hour))
	if err != nil || len(expired) != 1 || expired[0].ID != early.ID || expired[0].Status != StatusExpired {
		t.Fatalf("ExpirePending = %+v, %v", expired, err)
	}
	if got, _ := s.UserByID(ctx, early.ID); got.Status != StatusExpired || got.Secret != "" {
		t.Fatalf("expired user: %+v", got)
	}
	if got, _ := s.UserByID(ctx, late.ID); got.Status != StatusPending || got.Secret != "B" {
		t.Fatalf("pending user changed: %+v", got)
	}
	if again, err := s.ExpirePending(ctx, t0.Add(2*time.Hour)); err != nil || len(again) != 0 {
		t.Fatalf("second sweep = %+v, %v", again, err)
	}
}

func TestDeleteUserKeepsMessages(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	admin := mustCreate(t, s, NewUser{Name: "admin", Admin: true, Status: StatusActive})
	carol := mustCreate(t, s, NewUser{Name: "carol", Status: StatusActive})

	m, err := s.AddMessage(ctx, KindText, carol.ID, "carol", "hello", 20)
	if err != nil || m.UserID != carol.ID || m.AuthorDeleted {
		t.Fatalf("AddMessage = %+v, %v", m, err)
	}
	sys, _ := s.AddMessage(ctx, KindSystem, 0, SystemAuthor, "note", 20)

	if err := s.DeleteUser(ctx, carol.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(ctx, admin.ID); !errors.Is(err, ErrNoUser) {
		t.Fatalf("deleting the admin: %v", err)
	}
	got, err := s.Message(ctx, m.ID)
	if err != nil || got.UserID != 0 || !got.AuthorDeleted || got.Author != "carol" {
		t.Fatalf("message of a deleted user = %+v, %v", got, err)
	}
	if got, _ := s.Message(ctx, sys.ID); got.AuthorDeleted {
		t.Fatal("a system message is marked as from a deleted user")
	}

	// The name is free again, and the new carol does not own the old messages.
	carol2 := mustCreate(t, s, NewUser{Name: "Carol", Status: StatusActive})
	if carol2.ID == carol.ID {
		t.Fatal("user ids are reused")
	}
	if got, _ := s.Message(ctx, m.ID); got.UserID == carol2.ID {
		t.Fatal("the new user inherited the old messages")
	}
}

func TestOldSchemaRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	s, err := Create(path, testKey(5))
	if err != nil {
		t.Fatal(err)
	}
	// Pretend this is a v0.2 database.
	if _, err := s.db.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(path, testKey(5)); !errors.Is(err, ErrOldSchema) {
		t.Fatalf("Open v0.2 database: %v", err)
	}
}
