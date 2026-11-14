// Package auth implements cpsync's authentication building blocks:
//
//	limiter.go   per-IP lockout after repeated failures
//	sessions.go  in-memory browser sessions with an inactivity timeout
//	totp.go      TOTP secrets: QR codes, code validation, replay guard
//
// The package has no HTTP code; package server decides when to use these.
// Everything here is safe for concurrent use.
package auth

import (
	"sync"
	"time"
)

// Limiter locks out an IP address after too many failed attempts.
//
// Rules: max failures in a row lock the address for duration. A success
// clears the count. Failures older than duration are forgotten, so a
// typo now and then never adds up to a lockout. The counter restarts
// after a lockout.
//
// Attempts are reserved before the credential is checked (Begin), and
// settled afterwards (Attempt.Fail, Attempt.Succeed or Attempt.Cancel).
// A reserved attempt counts against the limit while it runs, so parallel
// requests cannot get more than max guesses evaluated: checking a
// credential can take a while (the admin password uses Argon2id), and if
// failures were only counted at the end, many requests could all pass the
// check first (issue #14).
type Limiter struct {
	max      int
	duration time.Duration
	now      func() time.Time // replaceable clock, for tests

	mu      sync.Mutex
	entries map[string]*limiterEntry
}

type limiterEntry struct {
	fails   int       // settled failures in a row
	pending int       // attempts reserved by Begin and not yet settled
	until   time.Time // locked out until
	latest  time.Time // last failure
}

// NewLimiter returns a limiter allowing max failures before locking an IP
// out for duration. Failures older than duration are forgotten.
func NewLimiter(max int, duration time.Duration) *Limiter {
	return &Limiter{max: max, duration: duration, now: time.Now, entries: map[string]*limiterEntry{}}
}

// Locked reports whether ip is locked out and for how much longer.
func (l *Limiter) Locked(ip string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[ip]
	if e == nil {
		return false, 0
	}
	if rem := e.until.Sub(l.now()); rem > 0 {
		return true, rem
	}
	return false, 0
}

// BusyRetry is how long a client is asked to wait when all its attempts
// are already being checked.
const BusyRetry = time.Second

// Attempt is one credential check reserved with Begin. Exactly one of
// Fail, Succeed or Cancel settles it; later calls do nothing, so Cancel
// can be deferred as a safety net.
type Attempt struct {
	l    *Limiter
	ip   string
	done bool
}

// Begin reserves an attempt for ip. It refuses (ok false) when ip is
// locked out, or when the attempts already in progress plus the recorded
// failures reach the limit; retryAfter then says how long to wait.
func (l *Limiter) Begin(ip string) (a *Attempt, retryAfter time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e := l.entries[ip]
	if e != nil {
		if rem := e.until.Sub(now); rem > 0 {
			return nil, rem, false
		}
		if e.pending == 0 && now.Sub(e.latest) > l.duration {
			e.fails = 0 // old failures are forgotten
		}
	} else {
		e = &limiterEntry{}
		l.entries[ip] = e
	}
	if e.fails+e.pending >= l.max {
		return nil, BusyRetry, false
	}
	e.pending++
	return &Attempt{l: l, ip: ip}, 0, true
}

// settle ends the attempt and returns its entry; l.mu must be held.
func (a *Attempt) settle() *limiterEntry {
	if a == nil || a.done {
		return nil
	}
	a.done = true
	e := a.l.entries[a.ip]
	if e != nil && e.pending > 0 {
		e.pending--
	}
	return e
}

// Fail records the attempt as a failure. It returns whether ip is now
// locked out and how many attempts are left before a lockout.
func (a *Attempt) Fail() (locked bool, remaining int) {
	if a == nil {
		return false, 0
	}
	l := a.l
	l.mu.Lock()
	defer l.mu.Unlock()
	e := a.settle()
	if e == nil {
		return false, 0
	}
	now := l.now()
	e.fails++
	e.latest = now
	if e.fails >= l.max {
		e.fails = 0
		e.until = now.Add(l.duration)
		return true, 0
	}
	return false, l.max - e.fails
}

// Succeed records the attempt as a success, which clears the failures
// recorded for ip. It does not lift a lockout that started meanwhile.
func (a *Attempt) Succeed() {
	if a == nil {
		return
	}
	a.l.mu.Lock()
	defer a.l.mu.Unlock()
	if e := a.settle(); e != nil {
		e.fails = 0
	}
}

// Cancel releases the attempt without counting it, for requests that
// ended before the credential was checked (bad request, client gone,
// server busy). It does nothing if the attempt was already settled.
func (a *Attempt) Cancel() {
	if a == nil {
		return
	}
	a.l.mu.Lock()
	defer a.l.mu.Unlock()
	a.settle()
}

// Prune forgets entries that are neither locked, recent nor in progress.
func (l *Limiter) Prune() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for ip, e := range l.entries {
		if e.pending == 0 && !now.Before(e.until) && (e.fails == 0 || now.Sub(e.latest) > l.duration) {
			delete(l.entries, ip)
		}
	}
}
