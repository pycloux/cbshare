package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pycloux/cpsync/internal/config"
	"github.com/pycloux/cpsync/internal/store"
	"github.com/pycloux/cpsync/internal/tlsutil"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Create(filepath.Join(t.TempDir(), "test.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestServeHTTPS(t *testing.T) {
	st := newTestStore(t)
	cfg := config.Default()
	cfg.Bind = "127.0.0.1"
	srv, err := New(cfg, st, log.New(io.Discard, "", 0), "test")
	if err != nil {
		t.Fatal(err)
	}

	res, err := tlsutil.Load(context.Background(), "", "", []string{"127.0.0.1"}, st)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.ServeListener(ctx, ln, res.Certificate) }()

	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	base := "https://" + ln.Addr().String()

	resp, err := client.Get(base + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if body["status"] != "ok" || resp.TLS == nil {
		t.Fatalf("health: %v", body)
	}

	resp, err = client.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	html, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(html), "cpsync") {
		t.Fatal("index page not served")
	}

	cancel()
	if err := <-done; err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
}
