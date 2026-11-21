// Package server implements cbshare's HTTPS server and JSON API.
//
// Files in this package:
//
//	server.go    Server type, routes, startup/shutdown, JSON helpers
//	security.go  security headers, same-origin and CSRF checks
//	auth.go      setup (PIN + QR code), sign-in, sessions
//	users.go     user management by the admin: create, new QR code, revoke, delete
//	chat.go      messages and files
//	events.go    live updates (Server-Sent Events)
//	admin.go     admin unlock, moderation rights, the admin's own QR code
//
// A request goes through these layers, outermost first:
//
//	securityHeaders -> sameOrigin -> ServeMux -> withSession/withUser/withAdmin -> handler -> store
//
// The middleware wrappers (with*) look up the session from its cookie,
// check the anti-CSRF token on state-changing requests, and put the session
// in the request context, where handlers read it with sessionFrom.
//
// Every change to the chat calls changed(), which bumps the chat revision
// and wakes the live-update streams; browsers then fetch the new list.
package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pycloux/cbshare/internal/auth"
	"github.com/pycloux/cbshare/internal/config"
	"github.com/pycloux/cbshare/internal/crypto"
	"github.com/pycloux/cbshare/internal/store"
	"github.com/pycloux/cbshare/web"
)

// maxConcurrentHashes is how many admin password checks may run at once.
// Each Argon2id verification uses 64 MiB of memory and keeps a CPU core
// busy for a fraction of a second; two keep the server responsive.
const maxConcurrentHashes = 2

// Server is a cbshare HTTPS server. It is safe for concurrent use: the
// session store, limiter and hub have their own locks, revision is atomic,
// and the fields below mu are guarded by mu.
type Server struct {
	cfg     config.Config
	store   *store.Store
	log     *log.Logger
	version string
	mux     *http.ServeMux

	sessions *auth.Sessions
	limiter  *auth.Limiter
	hub      *hub

	// hashSlots limits how many admin password checks (Argon2id, 64 MiB
	// each) run at once; hashWait is how long a check may wait for a slot.
	// verifyPassword is crypto.VerifyPassword, replaceable in tests.
	hashSlots      chan struct{}
	hashWait       time.Duration
	verifyPassword func(password, hash string) bool

	// revision increases on every change of the chat, so that clients can
	// tell whether they are up to date. It starts at the current time in
	// milliseconds rather than 0, so that a browser still holding a
	// revision from before a restart never mistakes it for the current one.
	revision atomic.Int64

	// now is the clock (time.Now, replaceable in tests to cross the
	// 15-minute setup and rotation windows and the 24-hour enrolment
	// window). Every time limit of the server uses it, never time.Now
	// directly. replay refuses one-time codes already used.
	// dummySecret is checked when a sign-in names an unknown user, so that
	// the response takes as long as for a real one.
	now         func() time.Time
	replay      *auth.ReplayGuard
	dummySecret string

	mu            sync.Mutex
	setupComplete bool               // the admin account exists
	pin           string             // setup PIN; empty once setup is done
	pending       *pendingEnrollment // secret shown by setup, awaiting confirmation
	rotation      *pendingRotation   // new admin secret shown to the admin, awaiting confirmation
}

// pendingEnrollment is the secret shown during first-time setup. It only
// becomes the admin account's secret when the browser that entered the PIN
// (holder of the setup cookie carrying token) confirms it has been scanned.
type pendingEnrollment struct {
	token   string
	secret  string
	expires time.Time
}

// New creates a server. cfg.Bind must already be resolved to an address.
// If the database holds no admin account yet, setup has never been
// completed and a new setup PIN is generated (printed by main).
func New(cfg config.Config, st *store.Store, logger *log.Logger, version string) (*Server, error) {
	s := &Server{
		cfg:      cfg,
		store:    st,
		log:      logger,
		version:  version,
		mux:      http.NewServeMux(),
		sessions: auth.NewSessions(cfg.SessionTimeout),
		limiter:  auth.NewLimiter(cfg.MaxFailedAttempts, cfg.LockoutDuration),
		hub:      newHub(),

		hashSlots:      make(chan struct{}, maxConcurrentHashes),
		hashWait:       10 * time.Second,
		verifyPassword: crypto.VerifyPassword,

		now:    time.Now,
		replay: auth.NewReplayGuard(),
	}
	dummy, err := auth.NewEnrollment("unknown")
	if err != nil {
		return nil, err
	}
	s.dummySecret = dummy.Secret
	_, err = st.AdminUser(context.Background())
	switch {
	case err == nil:
		s.setupComplete = true
	case errors.Is(err, store.ErrNoUser):
		if s.pin, err = crypto.GeneratePIN(cfg.PINLength); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("read users: %w", err)
	}
	s.revision.Store(time.Now().UnixMilli())
	s.routes()
	return s, nil
}

// SetupPIN returns the PIN that unlocks the first-time setup, or "" when
// setup is already done.
func (s *Server) SetupPIN() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pin
}

// routes registers every endpoint. The wrapper says who may call it:
//
//	(none)       anyone, e.g. the sign-in screens
//	withUser     a signed-in browser (every session belongs to a user)
//	withAdmin    a browser signed in as the admin account, with admin
//	             rights unlocked
//
// touch / noTouch says whether the request counts as user activity for the
// inactivity timeout: background requests (polling, the event stream) must
// not keep an idle session alive.
func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/health", s.handleHealth)

	s.mux.HandleFunc("GET /api/state", s.handleState)
	s.mux.HandleFunc("POST /api/setup/pin", s.handleSetupPIN)
	s.mux.HandleFunc("POST /api/setup/confirm", s.handleSetupConfirm)
	s.mux.HandleFunc("POST /api/login", s.handleLogin)
	s.mux.HandleFunc("POST /api/logout", s.handleLogout)

	s.mux.HandleFunc("GET /api/messages", s.withUser(noTouch, s.handleMessages))
	s.mux.HandleFunc("POST /api/messages", s.withUser(touch, s.handlePostMessage))
	s.mux.HandleFunc("PUT /api/messages/{id}", s.withUser(touch, s.handleEditMessage))
	s.mux.HandleFunc("DELETE /api/messages/{id}", s.withUser(touch, s.handleDeleteMessage))
	s.mux.HandleFunc("POST /api/files", s.withUser(touch, s.handleUpload))
	s.mux.HandleFunc("GET /api/files/{id}", s.withUser(touch, s.handleDownload))
	s.mux.HandleFunc("GET /api/events", s.withUser(noTouch, s.handleEvents))

	s.mux.HandleFunc("POST /api/admin/login", s.withUser(touch, s.handleAdminLogin))
	s.mux.HandleFunc("POST /api/admin/logout", s.withUser(touch, s.handleAdminLogout))
	s.mux.HandleFunc("POST /api/admin/rotate", s.withAdmin(s.handleRotate))
	s.mux.HandleFunc("POST /api/admin/rotate/cancel", s.withAdmin(s.handleRotateCancel))
	s.mux.HandleFunc("POST /api/admin/rotate/confirm", s.withAdmin(s.handleRotateConfirm))

	s.mux.HandleFunc("GET /api/admin/users", s.withAdmin(s.handleListUsers))
	s.mux.HandleFunc("POST /api/admin/users", s.withAdmin(s.handleCreateUser))
	s.mux.HandleFunc("POST /api/admin/users/{id}/qr", s.withAdmin(s.handleUserQR))
	s.mux.HandleFunc("POST /api/admin/users/{id}/revoke", s.withAdmin(s.handleRevokeUser))
	s.mux.HandleFunc("DELETE /api/admin/users/{id}", s.withAdmin(s.handleDeleteUser))

	// Everything else is the embedded front end (index.html, app.js, ...).
	s.mux.Handle("GET /", http.FileServerFS(web.Static()))
}

// Handler returns the server's HTTP handler: the routes wrapped in the
// security middleware (see security.go).
func (s *Server) Handler() http.Handler { return securityHeaders(sameOrigin(s.mux)) }

// Addr returns the listen address.
func (s *Server) Addr() string {
	return net.JoinHostPort(s.cfg.Bind, strconv.Itoa(s.cfg.Port))
}

// PublicHost returns the host:port users open in their browser. When the
// server listens on all interfaces (0.0.0.0) it reports the LAN address.
func (s *Server) PublicHost() string {
	host := s.cfg.Bind
	if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
		host = config.DetectLANIP()
	}
	return net.JoinHostPort(host, strconv.Itoa(s.cfg.Port))
}

// ServeListener serves HTTPS on ln until ctx is cancelled, then shuts down
// gracefully. The caller opens ln (main does, so that it can print
// "listening on" only once the port is open).
func (s *Server) ServeListener(ctx context.Context, ln net.Listener, cert tls.Certificate) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          log.New(io.Discard, "", 0), // TLS handshake noise from untrusted certificates
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		},
	}
	// Serve in the background and wait for either a serve error or ctx.
	errc := make(chan error, 1)
	go func() { errc <- srv.ServeTLS(ln, "", "") }()

	janitorCtx, stopJanitor := context.WithCancel(ctx)
	defer stopJanitor()
	go s.janitor(janitorCtx)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		s.sessions.EndAll() // closes event streams so Shutdown does not wait on them
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := srv.Shutdown(shutdownCtx)
		if errors.Is(err, context.DeadlineExceeded) {
			err = srv.Close()
		}
		<-errc
		return err
	}
}

// janitor expires inactive sessions and unused QR codes, and forgets old
// failed attempts.
func (s *Server) janitor(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n := s.sessions.Expire(); n > 0 {
				s.log.Printf("[auth] %d session(s) expired after %s of inactivity", n, s.cfg.SessionTimeout)
			}
			s.limiter.Prune()
			s.expireEnrolments(ctx)
		}
	}
}

// changed records a change of the chat. Handlers call it after every
// successful write so that browsers refresh: event streams are woken
// through the hub, and polling browsers see a new revision.
func (s *Server) changed() {
	s.revision.Add(1)
	s.hub.notify()
}

// systemMessage posts a message from cbshare itself to the chat, such as a
// failed sign-in alert. It counts toward the retention limit like any
// other message. Failures are only logged: an alert that cannot be stored
// must not break the request that triggered it.
func (s *Server) systemMessage(ctx context.Context, text string) {
	if _, err := s.store.AddMessage(ctx, store.KindSystem, 0, store.SystemAuthor, text, s.cfg.MaxMessages); err != nil {
		s.log.Printf("[chat] cannot post system message: %v", err)
		return
	}
	s.changed()
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.version})
}

// writeJSON sends v as JSON. API responses are never cached: they can
// contain secrets and change constantly.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// apiError is the JSON body of every error response. Error is a sentence
// the front end shows as is; Code is a stable identifier the front end
// uses to react (for example "unauthenticated" sends it to the sign-in
// screen).
type apiError struct {
	Error      string `json:"error"`
	Code       string `json:"code,omitempty"`
	RetryAfter int    `json:"retryAfter,omitempty"` // seconds
	Remaining  *int   `json:"remaining,omitempty"`  // attempts left before a lockout
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, apiError{Error: msg, Code: code})
}

// readJSON decodes a small JSON request body (at most 1 MB) into v. It
// writes the error response itself and returns false on failure, so
// handlers simply return. Requiring the application/json content type is
// part of the CSRF defence: HTML forms on other sites cannot send it.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if !isJSON(r) {
		writeError(w, http.StatusUnsupportedMediaType, "bad_request", "Send the request body as application/json.")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "The request body is not valid JSON.")
		return false
	}
	return true
}
