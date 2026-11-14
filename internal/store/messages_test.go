package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "m.db")
	s, err := Create(path, testKey(3))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestMessagesCRUD(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)

	m, err := s.AddMessage(ctx, KindText, 0, "alice", "echo hello\n\tindented", 20)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Message(ctx, m.ID)
	if err != nil || got.Body != "echo hello\n\tindented" || got.Author != "alice" || got.EditedAt != nil {
		t.Fatalf("Message = %+v, %v", got, err)
	}

	edited, err := s.EditMessage(ctx, m.ID, "echo bye")
	if err != nil || edited.Body != "echo bye" || edited.EditedAt == nil {
		t.Fatalf("EditMessage = %+v, %v", edited, err)
	}
	if _, err := s.EditMessage(ctx, 999, "x"); !errors.Is(err, ErrNoMessage) {
		t.Fatalf("edit missing: %v", err)
	}

	if err := s.DeleteMessage(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteMessage(ctx, m.ID); !errors.Is(err, ErrNoMessage) {
		t.Fatalf("delete twice: %v", err)
	}
	if _, err := s.Message(ctx, m.ID); !errors.Is(err, ErrNoMessage) {
		t.Fatalf("get deleted: %v", err)
	}
}

func TestFileMessages(t *testing.T) {
	ctx := context.Background()
	s, path := newStore(t)
	data := bytes.Repeat([]byte("FILE-CONTENT-"), 1000)

	m, err := s.AddFileMessage(ctx, 0, "bob", "the config", NewFile{Name: "app.conf", Mime: "text/plain", Data: data}, 20)
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != KindFile || m.File == nil || m.File.Size != int64(len(data)) {
		t.Fatalf("got %+v", m)
	}
	info, got, err := s.File(ctx, m.File.ID)
	if err != nil || !bytes.Equal(got, data) || info.Name != "app.conf" {
		t.Fatalf("File = %+v, %d bytes, %v", info, len(got), err)
	}
	list, _ := s.Messages(ctx)
	if len(list) != 1 || list[0].File == nil || list[0].File.Name != "app.conf" {
		t.Fatalf("Messages = %+v", list)
	}

	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, []byte("FILE-CONTENT-")) {
		t.Fatal("file content stored in clear")
	}

	if err := s.DeleteMessage(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.File(ctx, m.File.ID); !errors.Is(err, ErrNoMessage) {
		t.Fatalf("file must be deleted with its message: %v", err)
	}
}

func TestRetentionPurgesOldMessagesAndFiles(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)

	first, err := s.AddFileMessage(ctx, 0, "a", "", NewFile{Name: "f", Mime: "text/plain", Data: []byte("x")}, 3)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := s.AddMessage(ctx, KindText, 0, "a", fmt.Sprint(i), 3); err != nil {
			t.Fatal(err)
		}
	}
	list, _ := s.Messages(ctx)
	if len(list) != 3 || list[0].Body != "0" || list[2].Body != "2" {
		t.Fatalf("expected the 3 newest messages, got %+v", list)
	}
	if _, _, err := s.File(ctx, first.File.ID); !errors.Is(err, ErrNoMessage) {
		t.Fatal("file of a purged message must be deleted")
	}
	var n int
	s.db.QueryRow("SELECT count(*) FROM files").Scan(&n)
	if n != 0 {
		t.Fatalf("%d orphan files", n)
	}
}
