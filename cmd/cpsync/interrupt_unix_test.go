//go:build !windows

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/pycloux/cpsync/internal/crypto"
	"github.com/pycloux/cpsync/internal/store"
)

// TestBinaryInterruptedFirstStart presses Ctrl+C (SIGINT) at different
// moments of a first start of the real binary and checks that it is all or
// nothing: either the key was printed and the database opens with it, or
// no database file is left behind and a new first start succeeds.
func TestBinaryInterruptedFirstStart(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "cpsync")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	keyRe := regexp.MustCompile(`cpsync --key (\S+)`)

	// Interrupt as soon as the file appears (inside first start), and
	// after short delays (around the key and later).
	delays := []time.Duration{0, 0, 5 * time.Millisecond, 50 * time.Millisecond, 200 * time.Millisecond, time.Second, 3 * time.Second}
	incomplete := 0
	for i, delay := range delays {
		db := filepath.Join(t.TempDir(), "cpsync.db")
		cmd := exec.Command(bin, "--password", "pw", "--db", db,
			"--bind", "127.0.0.1", "--port", strconv.Itoa(freePort(t)))
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()

		// Wait for the database file to appear (first start under way).
		deadline := time.After(20 * time.Second)
	wait:
		for {
			select {
			case err := <-done:
				t.Fatalf("run %d: cpsync exited before Ctrl+C (%v):\n%s", i, err, out.String())
			case <-deadline:
				cmd.Process.Kill()
				t.Fatalf("run %d: the database file never appeared:\n%s", i, out.String())
			default:
			}
			if _, err := os.Stat(db); err == nil {
				break wait
			}
			time.Sleep(time.Millisecond)
		}
		time.Sleep(delay)
		cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			cmd.Process.Kill()
			t.Fatalf("run %d: cpsync did not stop after Ctrl+C:\n%s", i, out.String())
		}

		m := keyRe.FindStringSubmatch(out.String())
		if m == nil {
			incomplete++
			if !bytes.Contains(out.Bytes(), []byte("first start interrupted")) {
				t.Errorf("run %d: expected the interruption to be explained:\n%s", i, out.String())
			}
			if left, _ := os.ReadDir(filepath.Dir(db)); len(left) != 0 {
				t.Fatalf("run %d: interrupted before the key was printed, but files were left: %v\n%s", i, left, out.String())
			}
			continue
		}
		key, err := crypto.DecodeKey(m[1])
		if err != nil {
			t.Fatal(err)
		}
		st, err := store.Open(db, key)
		if err != nil {
			t.Fatalf("run %d: the key was printed but the database does not open: %v", i, err)
		}
		_, ok, _ := st.Setting(t.Context(), store.SettingAdminHash)
		st.Close()
		if !ok {
			t.Fatalf("run %d: the key was printed but the admin hash is missing", i)
		}
	}
	t.Logf("%d of %d runs were interrupted before the key was printed", incomplete, len(delays))
}
