package server

// Admin features. Only the admin account (created at first start) can
// unlock admin rights, with the admin password chosen at first start.
// Rights belong to the session: they end with it (sign-out, expiry,
// restart, or "Lock admin").
//
// The admin replaces their own QR code here in two steps (show, then
// confirm), so that they cannot lock themselves out by mistake. Other
// users' codes are managed in users.go.

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/pycloux/cbshare/internal/auth"
	"github.com/pycloux/cbshare/internal/store"
)

// pendingRotation is a new secret for the admin account, shown to the
// admin and waiting for confirmation from the same session. Only one rotation can be pending; a
// new request replaces it. It expires after 15 minutes.
type pendingRotation struct {
	session string
	secret  string
	expires time.Time
}

// withAdmin requires a session of the admin account with admin rights
// unlocked.
func (s *Server) withAdmin(h http.HandlerFunc) http.HandlerFunc {
	return s.withUser(touch, func(w http.ResponseWriter, r *http.Request) {
		if ss := sessionFrom(r); !ss.Admin || !ss.IsAdminUser {
			writeError(w, http.StatusForbidden, "admin_required", "Unlock admin with the admin password first.")
			return
		}
		h(w, r)
	})
}

// POST /api/admin/login {password} — unlock admin rights for this session.
// Only sessions of the admin account may try; others are refused before
// the password is looked at, without counting an attempt.
func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if !sessionFrom(r).IsAdminUser {
		writeError(w, http.StatusForbidden, "admin_only", "Only the admin account can unlock admin tools.")
		return
	}
	ip := clientIP(r)
	attempt, ok := s.beginAttempt(w, r)
	if !ok {
		return
	}
	defer attempt.Cancel() // no-op once the attempt is settled
	var req struct {
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	// Only the Argon2id hash of the password is stored (see firstStart in
	// cmd/cbshare). A failure here counts toward the lockout like a wrong
	// one-time code, because it is the same kind of guessing.
	hash, ok, err := s.store.Setting(r.Context(), store.SettingAdminHash)
	if err != nil {
		s.internalError(w, fmt.Errorf("read admin password hash: %w", err))
		return
	}
	if !ok {
		// Only a database whose first start did not complete lacks the
		// hash; firstStart removes such databases, so this is unexpected.
		s.internalError(w, errors.New("admin password hash missing from the database: admin cannot be unlocked"))
		return
	}
	// Argon2id is deliberately expensive (64 MiB, a fraction of a second of
	// CPU), so only a few verifications may run at once, whoever asks.
	release, ok := s.acquireHashSlot(w, r)
	if !ok {
		return
	}
	valid := s.verifyPassword(req.Password, string(hash))
	release()
	if !valid {
		s.authFailed(w, r, attempt, "admin password", "That password is not the admin password.")
		return
	}
	attempt.Succeed()
	ss := sessionFrom(r)
	s.sessions.SetAdmin(ss.Token, true)
	s.log.Printf("[admin] %q unlocked admin from %s", ss.Username, ip)
	writeJSON(w, http.StatusOK, map[string]bool{"admin": true})
}

// acquireHashSlot waits for one of the hashSlots, so that parallel admin
// password checks cannot exhaust the server's CPU and memory. It answers
// 503 if no slot frees up within hashWait, and gives up silently if the
// client goes away. The caller must call release once the hash is checked.
func (s *Server) acquireHashSlot(w http.ResponseWriter, r *http.Request) (release func(), ok bool) {
	timer := time.NewTimer(s.hashWait)
	defer timer.Stop()
	select {
	case s.hashSlots <- struct{}{}:
		return func() { <-s.hashSlots }, true
	case <-r.Context().Done():
		return nil, false
	case <-timer.C:
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusServiceUnavailable, apiError{
			Error:      "The server is busy checking other passwords. Try again in a few seconds.",
			Code:       "busy",
			RetryAfter: 5,
		})
		return nil, false
	}
}

// POST /api/admin/logout — give up admin rights.
func (s *Server) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	ss := sessionFrom(r)
	s.sessions.SetAdmin(ss.Token, false)
	s.mu.Lock()
	if s.rotation != nil && s.rotation.session == ss.Token {
		s.rotation = nil
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"admin": false})
}

// POST /api/admin/rotate — create a new secret for the admin account and
// show its QR code. Nothing changes until it is confirmed.
func (s *Server) handleRotate(w http.ResponseWriter, r *http.Request) {
	enr, err := auth.NewEnrollment(s.accountLabel(adminName))
	if err != nil {
		s.internalError(w, err)
		return
	}
	ss := sessionFrom(r)
	s.mu.Lock()
	s.rotation = &pendingRotation{session: ss.Token, secret: enr.Secret, expires: s.now().Add(enrollmentValid)}
	s.mu.Unlock()
	s.log.Printf("[admin] %q generated a new QR code for the admin account (not yet active)", ss.Username)
	writeJSON(w, http.StatusOK, enr)
}

// POST /api/admin/rotate/cancel — keep the current secret.
func (s *Server) handleRotateCancel(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.rotation = nil
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// POST /api/admin/rotate/confirm — switch the admin account to the new
// secret and sign out the admin's sessions. Other users are not affected.
func (s *Server) handleRotateConfirm(w http.ResponseWriter, r *http.Request) {
	ss := sessionFrom(r)
	s.mu.Lock()
	p := s.rotation
	if p == nil || p.session != ss.Token || s.now().After(p.expires) {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "rotation_expired", "This QR code has expired. Create a new one.")
		return
	}
	if err := s.store.SetUserSecret(r.Context(), ss.UserID, p.secret, nil); err != nil {
		s.mu.Unlock()
		s.internalError(w, err)
		return
	}
	s.rotation = nil
	s.mu.Unlock()
	s.replay.Forget(ss.UserID)

	// EndUser closes the sessions' Done channels, which sends "logout" on
	// each of the admin's open event streams.
	s.systemMessage(r.Context(), "The admin replaced their own authenticator code.")
	n := s.sessions.EndUser(ss.UserID)
	clearCookie(w, sessionCookie)
	s.log.Printf("[admin] %q replaced the admin account's secret; %d admin session(s) signed out", ss.Username, n)
	writeJSON(w, http.StatusOK, map[string]int{"signedOut": n})
}
