package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Message kinds.
const (
	KindText   = "text"
	KindFile   = "file"
	KindSystem = "system"
)

// SystemAuthor is the author recorded for system messages.
const SystemAuthor = "cpsync"

// ErrNoMessage is returned when a message or file does not exist.
var ErrNoMessage = errors.New("message not found")

// Message is a chat message. UserID is the account that posted it: 0 for
// system messages and for messages whose author has been deleted, which
// AuthorDeleted then reports.
type Message struct {
	ID            int64      `json:"id"`
	Kind          string     `json:"kind"`
	UserID        int64      `json:"userId,omitempty"`
	Author        string     `json:"author"`
	AuthorDeleted bool       `json:"authorDeleted,omitempty"`
	Body          string     `json:"body"`
	CreatedAt     time.Time  `json:"createdAt"`
	EditedAt      *time.Time `json:"editedAt,omitempty"`
	File          *FileInfo  `json:"file,omitempty"`
}

// FileInfo describes a file attached to a message, without its content.
type FileInfo struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Mime string `json:"mime"`
	Size int64  `json:"size"`
}

// NewFile is a file to attach to a new message.
type NewFile struct {
	Name string
	Mime string
	Data []byte
}

// AddMessage inserts a message posted by user userID under the name
// author (userID 0 for system messages) and keeps only the newest keep
// messages, deleting older ones together with their files.
func (s *Store) AddMessage(ctx context.Context, kind string, userID int64, author, body string, keep int) (Message, error) {
	return s.insert(ctx, kind, userID, author, body, nil, keep)
}

// AddFileMessage inserts a message with an attached file and applies the
// same retention as AddMessage.
func (s *Store) AddFileMessage(ctx context.Context, userID int64, author, caption string, f NewFile, keep int) (Message, error) {
	return s.insert(ctx, KindFile, userID, author, caption, &f, keep)
}

// nullID stores the id 0 as NULL.
func nullID(id int64) sql.NullInt64 { return sql.NullInt64{Int64: id, Valid: id != 0} }

// insert adds a message (and its file) and applies retention in a single
// transaction, so the chat never holds more than keep messages, even
// briefly, and a failed insert leaves nothing behind.
func (s *Store) insert(ctx context.Context, kind string, userID int64, author, body string, f *NewFile, keep int) (Message, error) {
	// Timestamps are stored in milliseconds; truncate now so the returned
	// message is identical to what a later read returns.
	now := time.UnixMilli(time.Now().UnixMilli())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Message{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		"INSERT INTO messages(kind, user_id, author, body, created_at) VALUES(?, ?, ?, ?, ?)",
		kind, nullID(userID), author, body, now.UnixMilli())
	if err != nil {
		return Message{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Message{}, err
	}
	m := Message{ID: id, Kind: kind, UserID: userID, Author: author, Body: body, CreatedAt: now}
	if f != nil {
		res, err := tx.ExecContext(ctx,
			"INSERT INTO files(message_id, name, mime, size, data) VALUES(?, ?, ?, ?, ?)",
			id, f.Name, f.Mime, len(f.Data), f.Data)
		if err != nil {
			return Message{}, err
		}
		fid, err := res.LastInsertId()
		if err != nil {
			return Message{}, err
		}
		m.File = &FileInfo{ID: fid, Name: f.Name, Mime: f.Mime, Size: int64(len(f.Data))}
	}
	if err := prune(ctx, tx, keep); err != nil {
		return Message{}, err
	}
	return m, tx.Commit()
}

// prune deletes every message but the newest keep; files go with them.
func prune(ctx context.Context, tx *sql.Tx, keep int) error {
	_, err := tx.ExecContext(ctx,
		"DELETE FROM messages WHERE id NOT IN (SELECT id FROM messages ORDER BY id DESC LIMIT ?)", keep)
	return err
}

// selectMessages reads messages with their file description (not the file
// content, which is only loaded for downloads).
const selectMessages = `
	SELECT m.id, m.kind, m.user_id, m.author, m.body, m.created_at, m.edited_at,
	       f.id, f.name, f.mime, f.size
	FROM messages m LEFT JOIN files f ON f.message_id = m.id`

// scanner is implemented by both *sql.Row and *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

// scanMessage reads one row of selectMessages; NULL columns from the LEFT
// JOIN mean the message has no file.
func scanMessage(row scanner) (Message, error) {
	var (
		m       Message
		created int64
		userID  sql.NullInt64
		edited  sql.NullInt64
		fid     sql.NullInt64
		fname   sql.NullString
		fmime   sql.NullString
		fsize   sql.NullInt64
	)
	if err := row.Scan(&m.ID, &m.Kind, &userID, &m.Author, &m.Body, &created, &edited, &fid, &fname, &fmime, &fsize); err != nil {
		return Message{}, err
	}
	m.UserID = userID.Int64
	m.AuthorDeleted = m.Kind != KindSystem && !userID.Valid
	m.CreatedAt = time.UnixMilli(created)
	if edited.Valid {
		t := time.UnixMilli(edited.Int64)
		m.EditedAt = &t
	}
	if fid.Valid {
		m.File = &FileInfo{ID: fid.Int64, Name: fname.String, Mime: fmime.String, Size: fsize.Int64}
	}
	return m, nil
}

// Messages returns all messages, oldest first.
func (s *Store) Messages(ctx context.Context) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, selectMessages+" ORDER BY m.id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	msgs := []Message{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// Message returns one message.
func (s *Store) Message(ctx context.Context, id int64) (Message, error) {
	m, err := scanMessage(s.db.QueryRowContext(ctx, selectMessages+" WHERE m.id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, ErrNoMessage
	}
	return m, err
}

// EditMessage replaces the text of a message and marks it as edited.
func (s *Store) EditMessage(ctx context.Context, id int64, body string) (Message, error) {
	res, err := s.db.ExecContext(ctx,
		"UPDATE messages SET body = ?, edited_at = ? WHERE id = ?", body, time.Now().UnixMilli(), id)
	if err != nil {
		return Message{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Message{}, ErrNoMessage
	}
	return s.Message(ctx, id)
}

// DeleteMessage deletes a message and its file.
func (s *Store) DeleteMessage(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM messages WHERE id = ?", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoMessage
	}
	return nil
}

// File returns a file's description and content.
func (s *Store) File(ctx context.Context, id int64) (FileInfo, []byte, error) {
	var (
		f    FileInfo
		data []byte
	)
	err := s.db.QueryRowContext(ctx, "SELECT id, name, mime, size, data FROM files WHERE id = ?", id).
		Scan(&f.ID, &f.Name, &f.Mime, &f.Size, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return FileInfo{}, nil, ErrNoMessage
	}
	return f, data, err
}
