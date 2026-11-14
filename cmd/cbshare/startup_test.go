package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pycloux/cbshare/internal/config"
	"github.com/pycloux/cbshare/internal/store"
)

// cancelAfter is a context whose Err reports cancellation from its n-th
// call on. firstStart checks ctx.Err() once before creating the database
// and once before printing the key, so n selects which step sees Ctrl+C.
type cancelAfter struct {
	context.Context
	n     int32
	calls atomic.Int32
}

func (c *cancelAfter) Err() error {
	if c.calls.Add(1) >= c.n {
		return context.Canceled
	}
	return nil
}

func firstStartConfig(t *testing.T) config.Config {
	cfg := config.Default()
	cfg.Database = filepath.Join(t.TempDir(), "cbshare.db")
	return cfg
}

func assertNoDatabase(t *testing.T, cfg config.Config) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(cfg.Database))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("an interrupted first start must leave nothing behind, found %v", entries)
	}
}

func TestFirstStartInterruptedBeforeCreate(t *testing.T) {
	cfg := firstStartConfig(t)
	ctx := &cancelAfter{Context: context.Background(), n: 1}
	if _, err := firstStart(ctx, cfg, "pw"); !errors.Is(err, errInterrupted) {
		t.Fatalf("got %v, want errInterrupted", err)
	}
	assertNoDatabase(t, cfg)
}

func TestFirstStartInterruptedAfterCreate(t *testing.T) {
	cfg := firstStartConfig(t)
	ctx := &cancelAfter{Context: context.Background(), n: 2}
	if _, err := firstStart(ctx, cfg, "pw"); !errors.Is(err, errInterrupted) {
		t.Fatalf("got %v, want errInterrupted", err)
	}
	assertNoDatabase(t, cfg)

	// The operator can simply start again.
	st, err := firstStart(context.Background(), cfg, "pw")
	if err != nil {
		t.Fatalf("first start after an interruption: %v", err)
	}
	defer st.Close()
	if _, ok, _ := st.Setting(context.Background(), store.SettingAdminHash); !ok {
		t.Fatal("admin hash missing")
	}
}

func TestLoopbackWarning(t *testing.T) {
	cases := []struct {
		bind     string
		autoBind bool
		warn     bool
	}{
		{"127.0.0.1", true, true},     // nothing found: warn
		{"127.0.0.1", false, false},   // chosen with --bind: respect it
		{"192.168.1.10", true, false}, // LAN address found
		{"0.0.0.0", false, false},
	}
	for _, c := range cases {
		w := loopbackWarning(c.bind, c.autoBind)
		if (w != "") != c.warn {
			t.Errorf("loopbackWarning(%q, %v) = %q", c.bind, c.autoBind, w)
		}
		if c.warn && !strings.Contains(w, "--bind") {
			t.Errorf("the warning should suggest --bind: %q", w)
		}
	}
}
