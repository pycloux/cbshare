package config

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultsAreValid(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Port != 8443 || c.MaxMessages != 20 || c.MaxFileSizeMB != 20 || c.Mode != ModeSSE ||
		c.PollInterval != 3*time.Second || c.MaxFailedAttempts != 5 ||
		c.LockoutDuration != 5*time.Minute || c.SessionTimeout != 30*time.Minute || c.PINLength != 6 {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cpsync.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPrecedenceFlagsOverFileOverDefaults(t *testing.T) {
	p := writeFile(t, "port: 9000\nmax_messages: 50\nmode: polling\npoll_interval: 5s\n")
	cfg, _, err := Parse([]string{"--config", p, "--port", "9100"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 9100 {
		t.Errorf("port: flag should win, got %d", cfg.Port)
	}
	if cfg.MaxMessages != 50 || cfg.Mode != ModePolling || cfg.PollInterval != 5*time.Second {
		t.Errorf("file values not applied: %+v", cfg)
	}
	if cfg.MaxFileSizeMB != 20 {
		t.Errorf("default not kept: %d", cfg.MaxFileSizeMB)
	}
}

func TestUnknownKeyRejected(t *testing.T) {
	p := writeFile(t, "prot: 9000\n")
	if _, _, err := Parse([]string{"--config", p}, io.Discard); err == nil {
		t.Fatal("expected an error for an unknown key")
	}
}

func TestEmptyFileKeepsDefaults(t *testing.T) {
	p := writeFile(t, "")
	cfg, _, err := Parse([]string{"--config", p}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg != Default() {
		t.Fatalf("got %+v", cfg)
	}
}

func TestValidation(t *testing.T) {
	cases := [][]string{
		{"--port", "0"},
		{"--mode", "websocket"},
		{"--max-messages", "0"},
		{"--tls-cert", "a.pem"},
		{"--poll-interval", "10ms"},
	}
	for _, args := range cases {
		if _, _, err := Parse(args, io.Discard); err == nil {
			t.Errorf("%v: expected a validation error", args)
		}
	}
}

func TestOptions(t *testing.T) {
	_, opts, err := Parse([]string{"--password", "s3cret"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Password != "s3cret" || opts.Key != "" {
		t.Fatalf("got %+v", opts)
	}
}

func TestDescribe(t *testing.T) {
	d := Default().Describe()
	for _, want := range []string{"8443", "self-signed", "sse", "20 MB"} {
		if !strings.Contains(d, want) {
			t.Errorf("Describe() missing %q", want)
		}
	}
}

func TestDetectLANIP(t *testing.T) {
	if ip := net.ParseIP(DetectLANIP()); ip == nil || ip.To4() == nil {
		t.Fatal("expected an IPv4 address")
	}
}

func TestMaxUsers(t *testing.T) {
	if Default().MaxUsers != 10 {
		t.Fatalf("default max_users = %d, want 10", Default().MaxUsers)
	}
	p := writeFile(t, "max_users: 4\n")
	cfg, _, err := Parse([]string{"--config", p}, io.Discard)
	if err != nil || cfg.MaxUsers != 4 {
		t.Fatalf("from file: %d, %v", cfg.MaxUsers, err)
	}
	cfg, _, err = Parse([]string{"--config", p, "--max-users", "7"}, io.Discard)
	if err != nil || cfg.MaxUsers != 7 {
		t.Fatalf("flag over file: %d, %v", cfg.MaxUsers, err)
	}
	for _, bad := range []string{"0", "101", "-1"} {
		_, _, err := Parse([]string{"--max-users", bad}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "max_users must be between 1 and 100") {
			t.Errorf("--max-users %s: %v", bad, err)
		}
	}
	for _, ok := range []string{"1", "100"} {
		if _, _, err := Parse([]string{"--max-users", ok}, io.Discard); err != nil {
			t.Errorf("--max-users %s: %v", ok, err)
		}
	}
	if !strings.Contains(Default().Describe(), "max users           10") {
		t.Error("Describe does not show max users")
	}
}
