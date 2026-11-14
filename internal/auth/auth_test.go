package auth

import (
	"strings"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

// fail runs one failed attempt for ip, as a handler does.
func fail(t *testing.T, l *Limiter, ip string) (locked bool, remaining int) {
	t.Helper()
	a, _, ok := l.Begin(ip)
	if !ok {
		t.Fatalf("Begin(%q) refused", ip)
	}
	return a.Fail()
}

// succeed runs one successful attempt for ip.
func succeed(t *testing.T, l *Limiter, ip string) {
	t.Helper()
	a, _, ok := l.Begin(ip)
	if !ok {
		t.Fatalf("Begin(%q) refused", ip)
	}
	a.Succeed()
}

func TestLimiterLocksAfterMaxFailures(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	l := NewLimiter(5, 5*time.Minute)
	l.now = c.now

	for i := 1; i <= 4; i++ {
		locked, remaining := fail(t, l, "10.0.0.1")
		if locked || remaining != 5-i {
			t.Fatalf("attempt %d: locked=%v remaining=%d", i, locked, remaining)
		}
	}
	if locked, _ := fail(t, l, "10.0.0.1"); !locked {
		t.Fatal("5th failure must lock the IP")
	}
	if locked, rem := l.Locked("10.0.0.1"); !locked || rem != 5*time.Minute {
		t.Fatalf("Locked = %v, %v", locked, rem)
	}
	if locked, _ := l.Locked("10.0.0.2"); locked {
		t.Fatal("other IPs must not be affected")
	}
	c.advance(5*time.Minute + time.Second)
	if locked, _ := l.Locked("10.0.0.1"); locked {
		t.Fatal("lockout must expire")
	}
	if _, remaining := fail(t, l, "10.0.0.1"); remaining != 4 {
		t.Fatalf("counter must restart after a lockout, remaining=%d", remaining)
	}
}

func TestLimiterSuccessResets(t *testing.T) {
	l := NewLimiter(3, time.Minute)
	fail(t, l, "a")
	fail(t, l, "a")
	succeed(t, l, "a")
	if _, remaining := fail(t, l, "a"); remaining != 2 {
		t.Fatalf("remaining=%d", remaining)
	}
}

func TestLimiterReservesParallelAttempts(t *testing.T) {
	l := NewLimiter(5, 5*time.Minute)

	// Ten requests arrive at once: only five may be checked.
	var running []*Attempt
	for i := 0; i < 10; i++ {
		a, retry, ok := l.Begin("10.0.0.1")
		if ok {
			running = append(running, a)
		} else if retry != BusyRetry {
			t.Fatalf("a busy refusal should ask to retry after %s, got %s", BusyRetry, retry)
		}
	}
	if len(running) != 5 {
		t.Fatalf("%d attempts allowed in parallel, want 5", len(running))
	}
	if locked, _ := l.Locked("10.0.0.1"); locked {
		t.Fatal("refusing extra attempts is not a lockout")
	}
	if _, _, ok := l.Begin("10.0.0.2"); !ok {
		t.Fatal("other IPs must not be affected")
	}

	// They all fail: the last one locks the address, nothing else runs.
	locked := false
	for _, a := range running {
		locked, _ = a.Fail()
	}
	if !locked {
		t.Fatal("five failures must lock the address")
	}
	if _, _, ok := l.Begin("10.0.0.1"); ok {
		t.Fatal("no attempt may start while locked")
	}
}

func TestLimiterCancelDoesNotCount(t *testing.T) {
	l := NewLimiter(2, time.Minute)
	for i := 0; i < 10; i++ {
		a, _, ok := l.Begin("a")
		if !ok {
			t.Fatalf("cancelled attempts must free their place (round %d)", i)
		}
		a.Cancel()
		a.Cancel() // twice is harmless
	}
	a, _, _ := l.Begin("a")
	a.Fail()
	a.Cancel() // after settling: no effect
	if _, remaining := fail(t, l, "a"); remaining != 0 {
		t.Fatalf("remaining=%d, want 0 (2 failures, Cancel must not undo one)", remaining)
	}
}

func TestLimiterRunningAttemptHoldsItsPlace(t *testing.T) {
	l := NewLimiter(3, time.Minute)
	slow, _, _ := l.Begin("a") // e.g. a correct password still being checked
	fail(t, l, "a")
	fail(t, l, "a")
	// 2 failures + 1 running = the limit: nothing else may start.
	if _, retry, ok := l.Begin("a"); ok || retry != BusyRetry {
		t.Fatalf("Begin = %v, %v; want a busy refusal", ok, retry)
	}
	slow.Succeed() // clears the failures
	if _, remaining := fail(t, l, "a"); remaining != 2 {
		t.Fatalf("remaining=%d, want 2 after the success", remaining)
	}
}

func TestLimiterPruneKeepsRunningAttempts(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	l := NewLimiter(2, time.Minute)
	l.now = c.now
	a, _, _ := l.Begin("a")
	c.advance(time.Hour)
	l.Prune()
	if locked, _ := a.Fail(); locked {
		t.Fatal("one failure must not lock")
	}
	if _, remaining := fail(t, l, "a"); remaining != 0 {
		t.Fatal("the running attempt must have been kept by Prune")
	}
	c.advance(time.Hour)
	l.Prune()
	l.mu.Lock()
	n := len(l.entries)
	l.mu.Unlock()
	if n != 0 {
		t.Fatalf("Prune kept %d entries", n)
	}
}

func TestSessionsLifecycle(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	s := NewSessions(30 * time.Minute)
	s.now = c.now

	ss, err := s.Create("10.0.0.1", User{ID: 7, Name: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if ss.Token == "" || ss.CSRF == "" || ss.Token == ss.CSRF {
		t.Fatal("tokens must be random and distinct")
	}
	if ss.UserID != 7 || ss.Username != "alice" || ss.IsAdminUser || ss.Admin {
		t.Fatalf("session user = %+v", ss)
	}

	c.advance(20 * time.Minute)
	got, ok := s.Get(ss.Token, false) // background request: no touch
	if !ok || got.Username != "alice" {
		t.Fatal("session should still be valid")
	}
	c.advance(11 * time.Minute)
	if _, ok := s.Get(ss.Token, true); ok {
		t.Fatal("background requests must not keep a session alive")
	}
	select {
	case <-ss.Done:
	default:
		t.Fatal("Done must be closed on expiry")
	}

	a, _ := s.Create("x", User{ID: 1, Name: "a"})
	b, _ := s.Create("y", User{ID: 2, Name: "b"})
	c.advance(20 * time.Minute)
	s.Get(a.Token, true) // user activity
	c.advance(20 * time.Minute)
	if n := s.Expire(); n != 1 {
		t.Fatalf("Expire ended %d sessions, want 1", n)
	}
	if _, ok := s.Get(b.Token, false); ok {
		t.Fatal("inactive session should be gone")
	}
	if n := s.EndAll(); n != 1 || s.Count() != 0 {
		t.Fatalf("EndAll ended %d", n)
	}
	<-a.Done
}

func TestTOTP(t *testing.T) {
	e, err := NewEnrollment("192.168.1.10")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(e.QR, "data:image/png;base64,") || !strings.HasPrefix(e.URI, "otpauth://totp/cpsync:") {
		t.Fatalf("unexpected enrollment %+v", e.URI)
	}
	now := time.Now()
	code, err := Code(e.Secret, now)
	if err != nil {
		t.Fatal(err)
	}
	if !ValidateCode(code, e.Secret, now) || !ValidateCode(code[:3]+" "+code[3:], e.Secret, now) {
		t.Fatal("current code rejected")
	}
	if !ValidateCode(code, e.Secret, now.Add(30*time.Second)) {
		t.Fatal("code from the previous window should be accepted")
	}
	if ValidateCode(code, e.Secret, now.Add(5*time.Minute)) {
		t.Fatal("old code accepted")
	}
	if ValidateCode("12345", e.Secret, now) || ValidateCode(code, "", now) {
		t.Fatal("malformed input accepted")
	}
}

func TestEndUser(t *testing.T) {
	s := NewSessions(30 * time.Minute)
	a1, _ := s.Create("x", User{ID: 1, Name: "a"})
	a2, _ := s.Create("y", User{ID: 1, Name: "a"})
	b, _ := s.Create("z", User{ID: 2, Name: "b", IsAdmin: true})
	if !b.IsAdminUser {
		t.Fatal("IsAdminUser not set")
	}
	if n := s.CountUser(1); n != 2 {
		t.Fatalf("CountUser = %d", n)
	}
	if n := s.EndUser(1); n != 2 {
		t.Fatalf("EndUser ended %d, want 2", n)
	}
	<-a1.Done
	<-a2.Done
	if _, ok := s.Get(b.Token, false); !ok {
		t.Fatal("another user's session was ended")
	}
	if n := s.EndUser(1); n != 0 {
		t.Fatalf("second EndUser ended %d", n)
	}
}

func TestValidateCodeStepAndReplay(t *testing.T) {
	e, err := NewEnrollment("alice@192.168.1.10:8443")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.URI, "alice@192.168.1.10") {
		t.Fatalf("label missing from %s", e.URI)
	}
	now := time.Unix(1_800_000_015, 0) // inside step 60_000_000
	code, _ := Code(e.Secret, now)
	step, ok := ValidateCodeStep(code, e.Secret, now)
	if !ok || step != 60_000_000 {
		t.Fatalf("step = %d, %v", step, ok)
	}
	// The same code 30 s later still validates, for the same step.
	if step2, ok := ValidateCodeStep(code, e.Secret, now.Add(30*time.Second)); !ok || step2 != step {
		t.Fatalf("previous-window step = %d, %v", step2, ok)
	}

	g := NewReplayGuard()
	if !g.Use(1, step) {
		t.Fatal("first use refused")
	}
	if g.Use(1, step) {
		t.Fatal("replayed code accepted")
	}
	if g.Use(1, step-1) {
		t.Fatal("older code accepted after a newer one")
	}
	if !g.Use(2, step) {
		t.Fatal("another user's code refused")
	}
	if !g.Use(1, step+1) {
		t.Fatal("next step refused")
	}
	g.Forget(1)
	if !g.Use(1, step) {
		t.Fatal("Forget did not reset the user")
	}
}
