package server

// Chat endpoints. The chat is small by design (max_messages, 20 by
// default), so the API always returns the whole list instead of paging or
// sending deltas: simpler for both sides, and still cheap.
//
// Ownership rule: a message belongs to the user account that sent it, by
// id, not by name: a name freed by deleting a user can be taken again, and
// the new user must not own the old messages. The admin may change any
// message (see loadOwnMessage).

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/pycloux/cbshare/internal/store"
)

const (
	maxTextBytes    = 256 << 10 // 256 KB per text message
	maxFileNameLen  = 200
	multipartMargin = 64 << 10 // room for multipart headers and the caption
)

// messagesResponse is the body of GET /api/messages. Revision lets the
// browser ask "anything new since rev?" cheaply.
type messagesResponse struct {
	Revision  int64           `json:"rev"`
	Unchanged bool            `json:"unchanged,omitempty"`
	Messages  []store.Message `json:"messages,omitempty"`
}

// GET /api/messages[?since=rev] — the whole chat (at most max_messages).
// With since equal to the current revision, only {"unchanged": true}.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	// Read the revision before the messages: if a change lands in between,
	// the browser gets newer data with an older revision and simply fetches
	// again on its next check. The reverse order could miss a change.
	rev := s.revision.Load()
	if since := r.URL.Query().Get("since"); since != "" && since == strconv.FormatInt(rev, 10) {
		writeJSON(w, http.StatusOK, messagesResponse{Revision: rev, Unchanged: true})
		return
	}
	msgs, err := s.store.Messages(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, messagesResponse{Revision: rev, Messages: msgs})
}

// POST /api/messages {text} — post a text message as the session's user.
func (s *Server) handlePostMessage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	text, ok := s.checkText(w, req.Text, false)
	if !ok {
		return
	}
	ss := sessionFrom(r)
	m, err := s.store.AddMessage(r.Context(), store.KindText, ss.UserID, ss.Username, text, s.cfg.MaxMessages)
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.changed()
	s.log.Printf("[chat] %q posted message #%d (%d bytes)", ss.Username, m.ID, len(text))
	writeJSON(w, http.StatusCreated, m)
}

// POST /api/files (multipart: file, caption) — share a file.
//
// The body is read part by part rather than with ParseMultipartForm, so
// that nothing is written to temporary files on disk in clear text: the
// file goes from the request straight into the encrypted database. Sizes
// are capped twice: the whole body (MaxBytesReader) and the file part.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	maxSize := s.cfg.MaxFileSize()
	r.Body = http.MaxBytesReader(w, r.Body, maxSize+multipartMargin)
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Send the file as multipart/form-data.")
		return
	}
	var (
		caption string
		file    *store.NewFile
	)
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			s.uploadError(w, err, maxSize)
			return
		}
		switch part.FormName() {
		case "caption":
			b, err := io.ReadAll(io.LimitReader(part, maxTextBytes+1))
			if err != nil {
				s.uploadError(w, err, maxSize)
				return
			}
			caption = string(b)
		case "file":
			if file != nil {
				writeError(w, http.StatusBadRequest, "bad_request", "Send one file at a time.")
				return
			}
			// Read one byte more than allowed to detect oversized files.
			data, err := io.ReadAll(io.LimitReader(part, maxSize+1))
			if err != nil {
				s.uploadError(w, err, maxSize)
				return
			}
			if int64(len(data)) > maxSize {
				s.tooLarge(w, maxSize)
				return
			}
			file = &store.NewFile{Name: cleanFileName(part.FileName()), Data: data}
			file.Mime = detectMime(file.Name, part.Header.Get("Content-Type"))
		}
		part.Close()
	}
	if file == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "No file was attached.")
		return
	}
	caption, ok := s.checkText(w, caption, true)
	if !ok {
		return
	}
	ss := sessionFrom(r)
	m, err := s.store.AddFileMessage(r.Context(), ss.UserID, ss.Username, caption, *file, s.cfg.MaxMessages)
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.changed()
	s.log.Printf("[chat] %q shared file %q (%d bytes) in message #%d", ss.Username, file.Name, len(file.Data), m.ID)
	writeJSON(w, http.StatusCreated, m)
}

// uploadError maps a read error during an upload to a response.
func (s *Server) uploadError(w http.ResponseWriter, err error, maxSize int64) {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		s.tooLarge(w, maxSize)
		return
	}
	writeError(w, http.StatusBadRequest, "bad_request", "The upload was interrupted. Try again.")
}

// tooLarge answers 413 with the configured limit in the message.
func (s *Server) tooLarge(w http.ResponseWriter, maxSize int64) {
	writeError(w, http.StatusRequestEntityTooLarge, "too_large",
		fmt.Sprintf("The file is larger than the %s limit.", formatSize(maxSize)))
}

// PUT /api/messages/{id} {text} — replace a message's text (or a file's
// caption). The message is marked as edited.
func (s *Server) handleEditMessage(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadOwnMessage(w, r, false)
	if !ok {
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	text, ok := s.checkText(w, req.Text, m.Kind == store.KindFile)
	if !ok {
		return
	}
	updated, err := s.store.EditMessage(r.Context(), m.ID, text)
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.changed()
	ss := sessionFrom(r)
	s.log.Printf("[chat] %q edited message #%d%s", ss.Username, m.ID, adminSuffix(ss.Admin, m.UserID != ss.UserID))
	writeJSON(w, http.StatusOK, updated)
}

// DELETE /api/messages/{id} — delete a message; its file goes with it.
func (s *Server) handleDeleteMessage(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadOwnMessage(w, r, true)
	if !ok {
		return
	}
	if err := s.store.DeleteMessage(r.Context(), m.ID); err != nil {
		s.storeError(w, err)
		return
	}
	s.changed()
	ss := sessionFrom(r)
	s.log.Printf("[chat] %q deleted message #%d%s", ss.Username, m.ID, adminSuffix(ss.Admin, m.UserID != ss.UserID))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// loadOwnMessage loads the message named in the path and checks that the
// session may change it: its author (same user id) or the admin. System
// messages can only be deleted, by the admin.
func (s *Server) loadOwnMessage(w http.ResponseWriter, r *http.Request, deleting bool) (store.Message, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "This message no longer exists.")
		return store.Message{}, false
	}
	m, err := s.store.Message(r.Context(), id)
	if err != nil {
		s.storeError(w, err)
		return store.Message{}, false
	}
	ss := sessionFrom(r)
	switch {
	case m.Kind == store.KindSystem && !deleting:
		writeError(w, http.StatusForbidden, "forbidden", "System messages cannot be edited.")
	case m.Kind == store.KindSystem && !ss.Admin:
		writeError(w, http.StatusForbidden, "forbidden", "Only the admin can delete system messages.")
	case m.UserID != ss.UserID && !ss.Admin:
		writeError(w, http.StatusForbidden, "forbidden", "You can only change your own messages.")
	default:
		return m, true
	}
	return store.Message{}, false
}

// GET /api/files/{id} — download an attachment.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "This file no longer exists.")
		return
	}
	info, data, err := s.store.File(r.Context(), id)
	if err != nil {
		s.storeError(w, err)
		return
	}
	h := w.Header()
	// Always a download, never rendered in the page: shared files are untrusted.
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": info.Name}))
	h.Set("Content-Length", strconv.Itoa(len(data)))
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.Write(data)
}

// storeError maps store errors to responses: a missing message is a 404
// (it may just have been deleted or purged by retention), anything else
// is an internal error.
func (s *Server) storeError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNoMessage) {
		writeError(w, http.StatusNotFound, "not_found", "This message no longer exists.")
		return
	}
	s.internalError(w, err)
}

// checkText validates a message text. Text is kept exactly as typed so
// that scripts and keys survive copy and paste.
func (s *Server) checkText(w http.ResponseWriter, text string, allowEmpty bool) (string, bool) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	switch {
	case !utf8.ValidString(text):
		writeError(w, http.StatusBadRequest, "invalid_text", "The message is not valid text.")
	case len(text) > maxTextBytes:
		writeError(w, http.StatusRequestEntityTooLarge, "too_large",
			fmt.Sprintf("The message is longer than %s. Share it as a file instead.", formatSize(maxTextBytes)))
	case !allowEmpty && strings.TrimSpace(text) == "":
		writeError(w, http.StatusBadRequest, "empty", "Write something before sending.")
	default:
		return text, true
	}
	return "", false
}

// cleanFileName keeps the base name and drops characters that are unsafe
// in file names or headers.
func cleanFileName(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`"<>:|?*`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || name == "/" {
		name = "file"
	}
	if utf8.RuneCountInString(name) > maxFileNameLen {
		ext := path.Ext(name)
		if utf8.RuneCountInString(ext) > 20 {
			ext = ""
		}
		runes := []rune(strings.TrimSuffix(name, ext))
		name = string(runes[:maxFileNameLen-utf8.RuneCountInString(ext)]) + ext
	}
	return name
}

// detectMime picks a MIME type for display purposes: from the extension,
// else from what the browser declared. It never affects how the file is
// served (always as an octet-stream download).
func detectMime(name, declared string) string {
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	if mt, _, err := mime.ParseMediaType(declared); err == nil && mt != "" {
		return mt
	}
	return "application/octet-stream"
}

// formatSize formats a byte count for error messages: "20 MB", "256 KB".
func formatSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%g MB", float64(n*10/(1<<20))/10)
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

// adminSuffix marks log lines for actions an admin took on someone else's
// message.
func adminSuffix(admin, notOwn bool) string {
	if admin && notOwn {
		return " as admin"
	}
	return ""
}
