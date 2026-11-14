package server

// User management by the admin (Admin tools → Users):
//
//	GET    /api/admin/users                 the users, with their status
//	POST   /api/admin/users       {name}    create a user; their QR code, shown once
//	POST   /api/admin/users/{id}/qr         new QR code: the old one stops working
//	POST   /api/admin/users/{id}/revoke     withdraw access
//	DELETE /api/admin/users/{id}            delete; their messages stay
//
// A new QR code must be used for a first sign-in within enrolmentWindow
// (24 hours), or it expires (janitor and handleLogin both check). QR codes
// are returned once, in the response that creates them, and never stored
// anywhere the browser could fetch them again. The admin passes them on to
// the user through a private channel.
//
// The admin account itself cannot be revoked or deleted, so admin access
// is never lost; the admin replaces their own code with /api/admin/rotate.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/pycloux/cpsync/internal/auth"
	"github.com/pycloux/cpsync/internal/store"
)

// userView is a user as the admin sees it in the list.
type userView struct {
	store.User
	Sessions int `json:"sessions"`
}

// enrolledUser is the response that carries a new QR code.
type enrolledUser struct {
	User       userView        `json:"user"`
	Enrollment auth.Enrollment `json:"enrollment"`
}

func (s *Server) view(u store.User) userView {
	return userView{User: u, Sessions: s.sessions.CountUser(u.ID)}
}

// GET /api/admin/users
func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	// Expire first, so that the list never shows a pending user whose
	// window has passed.
	s.expireEnrolments(r.Context())
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	views := make([]userView, len(users))
	for i, u := range users {
		views[i] = s.view(u)
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": views, "count": len(users), "max": s.cfg.MaxUsers})
}

// POST /api/admin/users {name} — create a user and return their QR code.
func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	name, err := cleanUsername(req.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_username", err.Error())
		return
	}
	enr, err := auth.NewEnrollment(s.accountLabel(name))
	if err != nil {
		s.internalError(w, err)
		return
	}
	now := s.now()
	u, err := s.store.CreateUser(r.Context(), store.NewUser{
		Name: name, Status: store.StatusPending, Secret: enr.Secret, EnrolExpiresAt: now.Add(enrolmentWindow),
	}, s.cfg.MaxUsers, now)
	switch {
	case errors.Is(err, store.ErrNameTaken):
		writeError(w, http.StatusConflict, "name_taken", fmt.Sprintf("There is already a user named %q.", name))
		return
	case errors.Is(err, store.ErrUserLimit):
		writeError(w, http.StatusConflict, "user_limit",
			fmt.Sprintf("cpsync is limited to %d users. Delete a user first, or raise max_users.", s.cfg.MaxUsers))
		return
	case err != nil:
		s.internalError(w, err)
		return
	}
	ss := sessionFrom(r)
	s.log.Printf("[users] %q created user %q; QR code valid until %s", ss.Username, u.Name, u.EnrolExpiresAt.Format(time.RFC3339))
	s.systemMessage(r.Context(), fmt.Sprintf("The admin added %s.", u.Name))
	writeJSON(w, http.StatusCreated, enrolledUser{User: s.view(u), Enrollment: enr})
}

// regularUser loads the user named in the path and refuses the admin
// account, which these endpoints never change.
func (s *Server) regularUser(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "This user no longer exists.")
		return store.User{}, false
	}
	u, err := s.store.UserByID(r.Context(), id)
	if errors.Is(err, store.ErrNoUser) {
		writeError(w, http.StatusNotFound, "not_found", "This user no longer exists.")
		return store.User{}, false
	}
	if err != nil {
		s.internalError(w, err)
		return store.User{}, false
	}
	if u.Admin {
		writeError(w, http.StatusForbidden, "admin_account",
			"The admin account cannot be changed here. Use \"Replace my QR code\" for your own code.")
		return store.User{}, false
	}
	return u, true
}

// POST /api/admin/users/{id}/qr — give a user a new QR code. The old one
// stops working at once and the user's sessions end; the new one waits for
// a first sign-in, like a new user's.
func (s *Server) handleUserQR(w http.ResponseWriter, r *http.Request) {
	u, ok := s.regularUser(w, r)
	if !ok {
		return
	}
	enr, err := auth.NewEnrollment(s.accountLabel(u.Name))
	if err != nil {
		s.internalError(w, err)
		return
	}
	expires := s.now().Add(enrolmentWindow)
	if err := s.store.SetUserSecret(r.Context(), u.ID, enr.Secret, &expires); err != nil {
		s.storeUserError(w, err)
		return
	}
	s.replay.Forget(u.ID)
	n := s.sessions.EndUser(u.ID)
	u, err = s.store.UserByID(r.Context(), u.ID)
	if err != nil {
		s.storeUserError(w, err)
		return
	}
	ss := sessionFrom(r)
	s.log.Printf("[users] %q gave %q a new QR code; %d session(s) signed out", ss.Username, u.Name, n)
	s.systemMessage(r.Context(), fmt.Sprintf("The admin gave %s a new authenticator code.", u.Name))
	writeJSON(w, http.StatusOK, enrolledUser{User: s.view(u), Enrollment: enr})
}

// POST /api/admin/users/{id}/revoke — withdraw a user's access. Their
// secret is erased and their sessions end; a new QR code restores access.
func (s *Server) handleRevokeUser(w http.ResponseWriter, r *http.Request) {
	u, ok := s.regularUser(w, r)
	if !ok {
		return
	}
	if err := s.store.RevokeUser(r.Context(), u.ID); err != nil {
		s.storeUserError(w, err)
		return
	}
	n := s.sessions.EndUser(u.ID)
	u, err := s.store.UserByID(r.Context(), u.ID)
	if err != nil {
		s.storeUserError(w, err)
		return
	}
	ss := sessionFrom(r)
	s.log.Printf("[users] %q revoked %q; %d session(s) signed out", ss.Username, u.Name, n)
	s.systemMessage(r.Context(), fmt.Sprintf("The admin revoked %s's access.", u.Name))
	writeJSON(w, http.StatusOK, map[string]any{"user": s.view(u), "signedOut": n})
}

// DELETE /api/admin/users/{id} — delete a user. Their messages stay,
// marked as from a deleted user, and their name can be used again.
func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	u, ok := s.regularUser(w, r)
	if !ok {
		return
	}
	// End the sessions first, so that no request of the user runs against
	// a deleted account.
	n := s.sessions.EndUser(u.ID)
	if err := s.store.DeleteUser(r.Context(), u.ID); err != nil {
		s.storeUserError(w, err)
		return
	}
	s.replay.Forget(u.ID)
	ss := sessionFrom(r)
	s.log.Printf("[users] %q deleted user %q; %d session(s) signed out", ss.Username, u.Name, n)
	s.systemMessage(r.Context(), fmt.Sprintf("The admin removed %s.", u.Name))
	writeJSON(w, http.StatusOK, map[string]int{"signedOut": n})
}

// storeUserError answers a store error from a user operation.
func (s *Server) storeUserError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNoUser) {
		writeError(w, http.StatusNotFound, "not_found", "This user no longer exists.")
		return
	}
	s.internalError(w, err)
}

// expireEnrolments marks as expired the QR codes not used within their
// window, and says so in the log and the chat.
func (s *Server) expireEnrolments(ctx context.Context) {
	users, err := s.store.ExpirePending(ctx, s.now())
	if err != nil {
		s.log.Printf("[users] cannot expire unused QR codes: %v", err)
		return
	}
	for _, u := range users {
		s.log.Printf("[users] the QR code of %q expired before its first sign-in", u.Name)
		s.systemMessage(ctx, fmt.Sprintf("The QR code of %s expired before it was used. The admin can create a new one.", u.Name))
	}
}
