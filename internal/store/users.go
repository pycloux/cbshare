package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// User statuses. Only pending users within their enrolment window and
// active users can sign in.
//
//	pending  created or given a new QR code; must sign in once before
//	         EnrolExpiresAt
//	active   has signed in at least once since the QR code was issued
//	revoked  access withdrawn by the admin; the secret is erased
//	expired  the enrolment window passed without a sign-in; the secret
//	         is erased
const (
	StatusPending = "pending"
	StatusActive  = "active"
	StatusRevoked = "revoked"
	StatusExpired = "expired"
)

var (
	// ErrNoUser is returned when a user does not exist.
	ErrNoUser = errors.New("user not found")
	// ErrNameTaken is returned by CreateUser when the name is in use,
	// whatever its letter case.
	ErrNameTaken = errors.New("this username is already in use")
	// ErrUserLimit is returned by CreateUser when the limit is reached.
	ErrUserLimit = errors.New("the maximum number of users is reached")
)

// User is a person who can sign in. Secret is the base32 TOTP secret, or
// empty once revoked or expired; it is never sent to browsers except in
// the one-time enrollment response.
type User struct {
	ID             int64      `json:"id"`
	Name           string     `json:"name"`
	Admin          bool       `json:"admin"`
	Status         string     `json:"status"`
	Secret         string     `json:"-"`
	EnrolExpiresAt *time.Time `json:"enrolExpiresAt,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`
	ActivatedAt    *time.Time `json:"activatedAt,omitempty"`
	LastLoginAt    *time.Time `json:"lastLoginAt,omitempty"`
}

// NewUser describes a user to create.
type NewUser struct {
	Name           string
	Admin          bool
	Status         string // StatusPending or StatusActive
	Secret         string
	EnrolExpiresAt time.Time // ignored unless Status is StatusPending
}

// NameKey is the form of a name used to compare names: names that differ
// only in letter case are the same name.
func NameKey(name string) string { return strings.ToLower(name) }

const selectUsers = `
	SELECT id, name, is_admin, status, totp_secret, enrol_expires_at, created_at, activated_at, last_login_at
	FROM users`

func scanUser(row scanner) (User, error) {
	var (
		u                          User
		secret                     sql.NullString
		created                    int64
		enrol, activated, lastSeen sql.NullInt64
	)
	if err := row.Scan(&u.ID, &u.Name, &u.Admin, &u.Status, &secret, &enrol, &created, &activated, &lastSeen); err != nil {
		return User{}, err
	}
	u.Secret = secret.String
	u.CreatedAt = time.UnixMilli(created)
	u.EnrolExpiresAt = optTime(enrol)
	u.ActivatedAt = optTime(activated)
	u.LastLoginAt = optTime(lastSeen)
	return u, nil
}

func optTime(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.UnixMilli(v.Int64)
	return &t
}

func nullTime(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UnixMilli(), Valid: true}
}

// CreateUser adds a user, unless the name is taken (ErrNameTaken) or max
// users already exist (ErrUserLimit). The check and the insert run in one
// transaction, so concurrent creations cannot exceed the limit.
func (s *Store) CreateUser(ctx context.Context, nu NewUser, max int, now time.Time) (User, error) {
	now = time.UnixMilli(now.UnixMilli())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	var n, taken int
	if err := tx.QueryRowContext(ctx, "SELECT count(*), count(CASE WHEN name_key = ? THEN 1 END) FROM users", NameKey(nu.Name)).Scan(&n, &taken); err != nil {
		return User{}, err
	}
	if taken > 0 {
		return User{}, ErrNameTaken
	}
	if n >= max {
		return User{}, ErrUserLimit
	}
	u := User{Name: nu.Name, Admin: nu.Admin, Status: nu.Status, Secret: nu.Secret, CreatedAt: now}
	if nu.Status == StatusPending {
		t := time.UnixMilli(nu.EnrolExpiresAt.UnixMilli())
		u.EnrolExpiresAt = &t
	} else {
		u.ActivatedAt = &now
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO users(name, name_key, is_admin, status, totp_secret, enrol_expires_at, created_at, activated_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		u.Name, NameKey(u.Name), u.Admin, u.Status, u.Secret, nullTime(u.EnrolExpiresAt), now.UnixMilli(), nullTime(u.ActivatedAt))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return User{}, ErrNameTaken
		}
		return User{}, err
	}
	if u.ID, err = res.LastInsertId(); err != nil {
		return User{}, err
	}
	return u, tx.Commit()
}

// UserByName returns the user with that name, whatever its letter case.
func (s *Store) UserByName(ctx context.Context, name string) (User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, selectUsers+" WHERE name_key = ?", NameKey(name)))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNoUser
	}
	return u, err
}

// UserByID returns one user.
func (s *Store) UserByID(ctx context.Context, id int64) (User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, selectUsers+" WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNoUser
	}
	return u, err
}

// AdminUser returns the admin account, or ErrNoUser before setup.
func (s *Store) AdminUser(ctx context.Context) (User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, selectUsers+" WHERE is_admin = 1"))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNoUser
	}
	return u, err
}

// ListUsers returns every user, the admin first, then by name.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, selectUsers+" ORDER BY is_admin DESC, name_key")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// CountUsers returns the number of users, whatever their status.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM users").Scan(&n)
	return n, err
}

// exec runs an UPDATE or DELETE on one user and reports ErrNoUser when no
// row matched.
func (s *Store) exec(ctx context.Context, query string, args ...any) error {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoUser
	}
	return nil
}

// SetUserSecret gives a user a new secret, replacing the old one at once.
// With a non-nil enrolExpires the user becomes pending until that time;
// with nil the user stays (or becomes) active, which is used when the admin
// replaces their own code.
func (s *Store) SetUserSecret(ctx context.Context, id int64, secret string, enrolExpires *time.Time) error {
	if enrolExpires != nil {
		return s.exec(ctx,
			"UPDATE users SET totp_secret = ?, status = ?, enrol_expires_at = ?, activated_at = NULL WHERE id = ?",
			secret, StatusPending, enrolExpires.UnixMilli(), id)
	}
	return s.exec(ctx,
		"UPDATE users SET totp_secret = ?, status = ?, enrol_expires_at = NULL WHERE id = ?",
		secret, StatusActive, id)
}

// RecordLogin notes a successful sign-in: a pending user becomes active.
// It returns whether the user was activated by this sign-in.
func (s *Store) RecordLogin(ctx context.Context, id int64, now time.Time) (activated bool, err error) {
	ms := now.UnixMilli()
	res, err := s.db.ExecContext(ctx,
		"UPDATE users SET status = ?, enrol_expires_at = NULL, activated_at = ?, last_login_at = ? WHERE id = ? AND status = ?",
		StatusActive, ms, ms, id, StatusPending)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return true, nil
	}
	return false, s.exec(ctx, "UPDATE users SET last_login_at = ? WHERE id = ?", ms, id)
}

// RevokeUser withdraws a user's access and erases their secret.
func (s *Store) RevokeUser(ctx context.Context, id int64) error {
	return s.exec(ctx,
		"UPDATE users SET status = ?, totp_secret = NULL, enrol_expires_at = NULL WHERE id = ? AND is_admin = 0",
		StatusRevoked, id)
}

// ExpirePending marks as expired the pending users whose enrolment window
// ended before now, erases their secrets, and returns them.
func (s *Store) ExpirePending(ctx context.Context, now time.Time) ([]User, error) {
	rows, err := s.db.QueryContext(ctx,
		`UPDATE users SET status = ?, totp_secret = NULL, enrol_expires_at = NULL
		 WHERE status = ? AND enrol_expires_at <= ?
		 RETURNING id, name, is_admin, status, totp_secret, enrol_expires_at, created_at, activated_at, last_login_at`,
		StatusExpired, StatusPending, now.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// DeleteUser removes a user. Their messages stay, attributed to their name
// and marked as from a deleted user; the name becomes free again. The
// admin account cannot be deleted.
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	return s.exec(ctx, "DELETE FROM users WHERE id = ? AND is_admin = 0", id)
}
