package server

// Authentication flow, as seen from the browser:
//
//	First start only:
//	  POST /api/setup/pin      {pin}            -> QR code of the admin account's secret
//	  POST /api/setup/confirm                   -> admin account created, setup closed for good
//	Every visit:
//	  POST /api/login          {username, code} -> session cookie + anti-CSRF token
//	  ... chat ...
//	  POST /api/logout
//
// GET /api/state tells the front end which of these screens to show.
//
// Every user has their own TOTP secret, created by the admin (users.go):
// a valid code proves who someone is, and their account name is the name
// shown next to their messages.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/pycloux/cbshare/internal/auth"
	"github.com/pycloux/cbshare/internal/crypto"
	"github.com/pycloux/cbshare/internal/store"
)

const (
	sessionCookie = "cbshare_session"
	setupCookie   = "cbshare_setup"

	maxUsernameLen = 32

	// qrConfirmWindow is how long the QR code shown at setup, or a new
	// QR code for the admin account, waits for its confirmation.
	qrConfirmWindow = 15 * time.Minute

	// adminName is the admin account's name, reserved for it.
	adminName = "admin"
)

// firstSignInWindow is how long a new user's QR code waits for its first
// sign-in before it expires. It is a variable only so that test builds can shorten
// it (testhooks.go).
var firstSignInWindow = 24 * time.Hour

// touch marks handlers whose requests count as user activity; background
// requests (polling, event streams) do not keep a session alive.
const (
	touch   = true
	noTouch = false
)

// ctxKey is the request-context key under which withSession stores the
// session. An unexported type cannot collide with keys of other packages.
type ctxKey struct{}

// sessionFrom returns the session attached to the request by withSession.
func sessionFrom(r *http.Request) auth.Session {
	ss, _ := r.Context().Value(ctxKey{}).(auth.Session)
	return ss
}

// withUser requires a valid session cookie, and the session's anti-CSRF
// token on state-changing requests. On success the session is available to
// h through sessionFrom. touchSession resets the inactivity timer.
func (s *Server) withUser(touchSession bool, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ss, ok := s.currentSession(r, touchSession)
		if !ok {
			clearCookie(w, sessionCookie)
			writeError(w, http.StatusUnauthorized, "unauthenticated", "Your session has ended. Sign in again.")
			return
		}
		if !checkCSRF(w, r, ss.CSRF) {
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, ss)))
	}
}

// currentSession looks up the session named by the request's cookie.
func (s *Server) currentSession(r *http.Request, touchSession bool) (auth.Session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return auth.Session{}, false
	}
	return s.sessions.Get(c.Value, touchSession)
}

// setupDone reports whether the admin account exists, i.e. whether the
// first-time setup has been completed.
func (s *Server) setupDone() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setupComplete
}

// stateResponse tells the browser which screen to show.
type stateResponse struct {
	Version        string `json:"version"`
	SetupRequired  bool   `json:"setupRequired"`
	Authenticated  bool   `json:"authenticated"`
	UserID         int64  `json:"userId,omitempty"`
	Username       string `json:"username,omitempty"`
	IsAdminUser    bool   `json:"isAdminUser"`
	Admin          bool   `json:"admin"`
	CSRF           string `json:"csrf,omitempty"`
	Mode           string `json:"mode"`
	PollIntervalMs int64  `json:"pollIntervalMs"`
	MaxFileSize    int64  `json:"maxFileSize"`
	MaxMessages    int    `json:"maxMessages"`
	MaxUsers       int    `json:"maxUsers"`
	SessionTimeout int64  `json:"sessionTimeoutMs"`
}

// GET /api/state — open to everyone. Reveals no secret except the
// anti-CSRF token of the caller's own session, which the page needs after
// a reload (the session cookie itself is HttpOnly).
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	resp := stateResponse{
		Version:        s.version,
		SetupRequired:  !s.setupDone(),
		Mode:           s.cfg.Mode,
		PollIntervalMs: s.cfg.PollInterval.Milliseconds(),
		MaxFileSize:    s.cfg.MaxFileSize(),
		MaxMessages:    s.cfg.MaxMessages,
		MaxUsers:       s.cfg.MaxUsers,
		SessionTimeout: s.cfg.SessionTimeout.Milliseconds(),
	}
	if ss, ok := s.currentSession(r, noTouch); ok {
		resp.Authenticated = true
		resp.UserID = ss.UserID
		resp.Username = ss.Username
		resp.IsAdminUser = ss.IsAdminUser
		resp.Admin = ss.Admin
		resp.CSRF = ss.CSRF
	}
	writeJSON(w, http.StatusOK, resp)
}

// beginAttempt reserves a credential check for the client's IP address,
// or answers 429 and returns false. Handlers call it before looking at the
// credential, so that a locked-out client learns nothing, not even whether
// its guess was right, and settle the attempt afterwards with authFailed
// or Attempt.Succeed (deferring Attempt.Cancel for every other outcome).
//
// The reservation counts against the limit while the check runs, so
// parallel requests cannot get more guesses evaluated than allowed: those
// beyond the limit are refused with "busy" until the running ones finish.
func (s *Server) beginAttempt(w http.ResponseWriter, r *http.Request) (*auth.Attempt, bool) {
	ip := clientIP(r)
	a, wait, ok := s.limiter.Begin(ip)
	if ok {
		return a, true
	}
	if locked, _ := s.limiter.Locked(ip); locked {
		writeJSON(w, http.StatusTooManyRequests, apiError{
			Error:      fmt.Sprintf("Too many failed attempts. Try again in %s.", humanDuration(wait)),
			Code:       "locked",
			RetryAfter: int(math.Ceil(wait.Seconds())),
		})
	} else {
		writeJSON(w, http.StatusTooManyRequests, apiError{
			Error:      "Too many attempts at once. Wait for the previous ones, then try again.",
			Code:       "busy",
			RetryAfter: int(math.Ceil(wait.Seconds())),
		})
	}
	return nil, false
}

// authFailed records a failed attempt, tells everyone in the chat, and
// answers the client. Announcing failures with the IP address is a
// deliberate feature: in a small group, people notice an unexpected
// attempt. The response says how many attempts remain before a lockout.
func (s *Server) authFailed(w http.ResponseWriter, r *http.Request, a *auth.Attempt, what, msg string) {
	ip := clientIP(r)
	locked, remaining := a.Fail()
	text := fmt.Sprintf("Failed sign-in attempt from %s: wrong %s.", ip, what)
	if locked {
		text += fmt.Sprintf(" %s is locked out for %s.", ip, humanDuration(s.cfg.LockoutDuration))
	}
	s.log.Printf("[auth] failed %s from %s (locked=%v)", what, ip, locked)
	s.systemMessage(r.Context(), text)

	if locked {
		writeJSON(w, http.StatusTooManyRequests, apiError{
			Error:      fmt.Sprintf("Too many failed attempts. Try again in %s.", humanDuration(s.cfg.LockoutDuration)),
			Code:       "locked",
			RetryAfter: int(s.cfg.LockoutDuration.Seconds()),
		})
		return
	}
	writeJSON(w, http.StatusUnauthorized, apiError{Error: msg, Code: "invalid", Remaining: &remaining})
}

// POST /api/setup/pin {pin} — first-time setup, step 1: unlock with the PIN
// printed in the console and receive the QR code of the admin account.
func (s *Server) handleSetupPIN(w http.ResponseWriter, r *http.Request) {
	if s.setupDone() {
		writeError(w, http.StatusConflict, "setup_done", "Setup is already complete. Sign in with your authenticator code.")
		return
	}
	ip := clientIP(r)
	attempt, ok := s.beginAttempt(w, r)
	if !ok {
		return
	}
	defer attempt.Cancel() // no-op once the attempt is settled
	var req struct {
		PIN string `json:"pin"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	s.mu.Lock()
	pin := s.pin
	s.mu.Unlock()
	// Constant-time comparison; PINs are case-insensitive for convenience.
	if !crypto.EqualFold(strings.TrimSpace(req.PIN), pin) {
		s.authFailed(w, r, attempt, "setup PIN", "That PIN is not correct. Check the server console.")
		return
	}
	attempt.Succeed()

	enr, err := auth.NewEnrollment(s.accountLabel(adminName))
	if err != nil {
		s.internalError(w, err)
		return
	}
	token, err := crypto.RandomToken()
	if err != nil {
		s.internalError(w, err)
		return
	}
	// Nothing is stored yet: the secret only becomes permanent when this
	// browser confirms it scanned the code. If someone else enters the PIN
	// meanwhile, their enrollment replaces this one.
	s.mu.Lock()
	s.pending = &pendingEnrollment{token: token, secret: enr.Secret, expires: s.now().Add(qrConfirmWindow)}
	s.mu.Unlock()

	setCookie(w, setupCookie, token)
	s.log.Printf("[auth] setup PIN accepted from %s, QR code shown", ip)
	writeJSON(w, http.StatusOK, enr)
}

// POST /api/setup/confirm — first-time setup, step 2: the admin has scanned
// the QR code; the admin account is created with that secret.
func (s *Server) handleSetupConfirm(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(setupCookie)
	// Hold the lock for the whole check-and-save so that two confirmations
	// cannot race.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setupComplete {
		writeError(w, http.StatusConflict, "setup_done", "Setup is already complete.")
		return
	}
	p := s.pending
	if err != nil || p == nil || !crypto.Equal(c.Value, p.token) || s.now().After(p.expires) {
		writeError(w, http.StatusForbidden, "setup_expired", "This setup session has expired. Enter the PIN again.")
		return
	}
	nu := store.NewUser{Name: adminName, Admin: true, Status: store.StatusActive, Secret: p.secret}
	if _, err := s.store.CreateUser(r.Context(), nu, s.cfg.MaxUsers, s.now()); err != nil {
		s.internalError(w, fmt.Errorf("create the admin account: %w", err))
		return
	}
	s.setupComplete, s.pin, s.pending = true, "", nil
	clearCookie(w, setupCookie)
	s.log.Printf("[auth] setup complete: admin account created (confirmed from %s)", clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "username": adminName})
}

// accountLabel is how an account appears in authenticator apps:
// "alice@192.168.1.10:8443".
func (s *Server) accountLabel(name string) string {
	return name + "@" + s.PublicHost()
}

// POST /api/login {username, code} — sign in with the current code from
// the user's own authenticator entry.
//
// Every failure (unknown name, revoked or expired account, wrong or reused
// code) gets the same answer, so that the response does not tell which
// names exist. An unknown name is checked against a dummy secret, so that
// it takes as long as a real one.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.setupDone() {
		writeError(w, http.StatusConflict, "setup_required", "The server is not set up yet.")
		return
	}
	ip := clientIP(r)
	attempt, ok := s.beginAttempt(w, r)
	if !ok {
		return
	}
	defer attempt.Cancel() // no-op once the attempt is settled
	var req struct {
		Username string `json:"username"`
		Code     string `json:"code"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	name := strings.Join(strings.Fields(req.Username), " ")
	now := s.now()
	user, err := s.store.UserByName(r.Context(), name)
	if err != nil && !errors.Is(err, store.ErrNoUser) {
		s.internalError(w, err)
		return
	}
	secret := s.dummySecret
	if err == nil && canSignIn(user, now) {
		secret = user.Secret
	}
	step, valid := auth.ValidateCodeStep(req.Code, secret, now)
	valid = valid && secret != s.dummySecret
	if valid && !s.replay.Use(user.ID, step) {
		valid = false
		s.log.Printf("[auth] reused one-time code for %q from %s", user.Name, ip)
	}
	if !valid {
		s.log.Printf("[auth] failed sign-in as %q from %s", truncate(name, maxUsernameLen), ip)
		s.authFailed(w, r, attempt, "username or code", "That username or code is not valid. Enter your username and the current 6-digit code from your authenticator app.")
		return
	}
	attempt.Succeed()
	activated, err := s.store.RecordLogin(r.Context(), user.ID, now)
	if err != nil {
		s.internalError(w, err)
		return
	}
	// Always issue a fresh session token after authentication, ending any
	// previous one (protects against session fixation).
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.End(c.Value)
	}
	ss, err := s.sessions.Create(ip, auth.User{ID: user.ID, Name: user.Name, IsAdmin: user.Admin})
	if err != nil {
		s.internalError(w, err)
		return
	}
	// The token travels only in an HttpOnly cookie; the CSRF token is
	// returned in the body because the page must send it back in a header.
	setCookie(w, sessionCookie, ss.Token)
	if activated {
		s.log.Printf("[users] %q signed in for the first time from %s", user.Name, ip)
	}
	s.log.Printf("[auth] sign-in as %q from %s", user.Name, ip)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "csrf": ss.CSRF, "username": user.Name})
}

// canSignIn reports whether user may sign in at now: active users, and
// pending users within their enrolment window.
func canSignIn(u store.User, now time.Time) bool {
	switch u.Status {
	case store.StatusActive:
		return u.Secret != ""
	case store.StatusPending:
		return u.Secret != "" && u.EnrolExpiresAt != nil && now.Before(*u.EnrolExpiresAt)
	}
	return false
}

// truncate shortens s to at most n characters, for logging names typed by
// unknown clients.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// POST /api/logout — ends the session. Open to everyone and idempotent, so
// a browser whose session already expired can still "sign out" cleanly.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.End(c.Value)
	}
	clearCookie(w, sessionCookie)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// cleanUsername collapses runs of spaces, enforces the length limit,
// rejects characters that cannot be displayed, and keeps the reserved
// names: the system author's, so nobody can impersonate system messages,
// and the admin account's.
func cleanUsername(raw string) (string, error) {
	name := strings.Join(strings.Fields(raw), " ")
	if name == "" {
		return "", fmt.Errorf("Enter a username.")
	}
	if utf8.RuneCountInString(name) > maxUsernameLen {
		return "", fmt.Errorf("Use %d characters or fewer.", maxUsernameLen)
	}
	for _, c := range name {
		if !unicode.IsPrint(c) {
			return "", fmt.Errorf("The username contains characters that cannot be displayed.")
		}
	}
	if strings.EqualFold(name, store.SystemAuthor) {
		return "", fmt.Errorf("%q is reserved for system messages.", store.SystemAuthor)
	}
	if strings.EqualFold(name, adminName) {
		return "", fmt.Errorf("%q is reserved for the admin account.", adminName)
	}
	return name, nil
}

// setCookie sets a session-scoped cookie:
//   - no Expires/Max-Age, so it disappears when the browser closes;
//   - Secure, so it is only sent over HTTPS;
//   - HttpOnly, so page scripts cannot read it;
//   - SameSite=Strict, so other sites cannot make the browser send it.
func setCookie(w http.ResponseWriter, name, value string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
}

// clearCookie tells the browser to delete a cookie.
func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
}

// clientIP returns the address of the TCP peer. Headers such as
// X-Forwarded-For are deliberately ignored: cbshare is reached directly on
// the LAN, and trusting such headers would let a client pick the address
// that gets locked out.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// internalError logs the real error on the console and returns a generic
// message, so that internal details never reach the browser.
func (s *Server) internalError(w http.ResponseWriter, err error) {
	s.log.Printf("[error] %v", err)
	writeError(w, http.StatusInternalServerError, "internal", "Something went wrong on the server. Check its console.")
}

// humanDuration formats durations such as "5 minutes" or "30 seconds".
func humanDuration(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return plural(int(math.Ceil(d.Hours())), "hour")
	case d >= time.Minute:
		return plural(int(math.Ceil(d.Minutes())), "minute")
	default:
		return plural(max(1, int(math.Ceil(d.Seconds()))), "second")
	}
}

// plural returns "1 minute" or "5 minutes".
func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
