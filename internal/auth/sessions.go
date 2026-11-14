package auth

import (
	"sync"
	"time"

	"github.com/pycloux/cpsync/internal/crypto"
)

// User identifies the account a session belongs to.
type User struct {
	ID      int64
	Name    string
	IsAdmin bool // the admin account (admin rights still need unlocking)
}

// Session is a snapshot of a browser session. Methods of Sessions return
// copies, so callers can read the fields without locking; a change made
// later (for example SetAdmin) is visible on the next Get.
//
// UserID and Username are the signed-in account; IsAdminUser says whether
// it is the admin account, and Admin whether admin rights are unlocked in
// this session.
type Session struct {
	Token       string
	CSRF        string
	IP          string
	UserID      int64
	Username    string
	IsAdminUser bool
	Admin       bool
	Created     time.Time
	LastSeen    time.Time
	// Done is closed when the session ends (logout, expiry or revocation).
	Done <-chan struct{}
}

// session is the stored form: the public snapshot plus the writable end
// of the Done channel.
type session struct {
	Session
	done chan struct{}
}

// Sessions keeps browser sessions in memory, indexed by their token.
// Sessions are never written to disk: restarting the server or closing the
// browser ends them.
//
// Expiry happens in two places: Get ends a session it finds expired, and
// Expire (called periodically by the server) ends the ones nobody asks
// about, so that their event streams are told to sign out.
type Sessions struct {
	timeout time.Duration
	now     func() time.Time

	mu sync.Mutex
	m  map[string]*session
}

// NewSessions returns a session store with the given inactivity timeout.
func NewSessions(timeout time.Duration) *Sessions {
	return &Sessions{timeout: timeout, now: time.Now, m: map[string]*session{}}
}

// Create starts a new session for user, signed in from ip.
func (s *Sessions) Create(ip string, user User) (Session, error) {
	token, err := crypto.RandomToken()
	if err != nil {
		return Session{}, err
	}
	csrf, err := crypto.RandomToken()
	if err != nil {
		return Session{}, err
	}
	now := s.now()
	done := make(chan struct{})
	ss := &session{Session: Session{
		Token: token, CSRF: csrf, IP: ip,
		UserID: user.ID, Username: user.Name, IsAdminUser: user.IsAdmin,
		Created: now, LastSeen: now, Done: done,
	}, done: done}
	s.mu.Lock()
	s.m[token] = ss
	s.mu.Unlock()
	return ss.Session, nil
}

// Get returns the session for token. When touch is true the inactivity
// timer is reset; background requests (polling, event streams) pass false.
func (s *Sessions) Get(token string, touch bool) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.m[token]
	if ss == nil {
		return Session{}, false
	}
	now := s.now()
	if now.Sub(ss.LastSeen) > s.timeout {
		s.endLocked(token)
		return Session{}, false
	}
	if touch {
		ss.LastSeen = now
	}
	return ss.Session, true
}

// SetAdmin grants or removes admin rights for a session.
func (s *Sessions) SetAdmin(token string, admin bool) bool {
	return s.update(token, func(ss *session) { ss.Admin = admin })
}

func (s *Sessions) update(token string, f func(*session)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.m[token]
	if ss == nil {
		return false
	}
	f(ss)
	return true
}

// End terminates one session.
func (s *Sessions) End(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endLocked(token)
}

// EndUser terminates every session of one user and returns how many were
// ended. It is used when the user's access is revoked, their code is
// replaced, or the user is deleted.
func (s *Sessions) EndUser(userID int64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for token, ss := range s.m {
		if ss.UserID == userID {
			s.endLocked(token)
			n++
		}
	}
	return n
}

// CountUser returns the number of active sessions of one user.
func (s *Sessions) CountUser(userID int64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	n := 0
	for _, ss := range s.m {
		if ss.UserID == userID && now.Sub(ss.LastSeen) <= s.timeout {
			n++
		}
	}
	return n
}

// EndAll terminates every session and returns how many were ended.
func (s *Sessions) EndAll() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.m)
	for token := range s.m {
		s.endLocked(token)
	}
	return n
}

// Expire ends the sessions inactive for longer than the timeout.
func (s *Sessions) Expire() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	n := 0
	for token, ss := range s.m {
		if now.Sub(ss.LastSeen) > s.timeout {
			s.endLocked(token)
			n++
		}
	}
	return n
}

// Count returns the number of active sessions.
func (s *Sessions) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// endLocked closes the session's Done channel (waking anything waiting on
// it, such as an event stream) and forgets it. s.mu must be held.
func (s *Sessions) endLocked(token string) {
	if ss := s.m[token]; ss != nil {
		close(ss.done)
		delete(s.m, token)
	}
}
