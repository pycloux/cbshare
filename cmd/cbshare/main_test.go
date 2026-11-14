package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pycloux/cbshare/internal/auth"
)

// TestBinaryEndToEnd builds the cbshare executable and drives it like a
// user: first start, setup, sign-in, a message, a restart with the key,
// and a second sign-in with the same authenticator secret.
func TestBinaryEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "cbshare")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	db := filepath.Join(dir, "data", "cbshare.db")
	port := freePort(t)
	base := "https://127.0.0.1:" + strconv.Itoa(port)
	common := []string{"--bind", "127.0.0.1", "--port", strconv.Itoa(port), "--db", db}

	// First start.
	p := startBinary(t, bin, append([]string{"--password", "correct horse"}, common...)...)
	out := p.waitFor(t, "listening on")
	mustAccept(t, port) // the line is printed only once the port is open
	key := regexp.MustCompile(`cbshare --key (\S+)`).FindStringSubmatch(out)
	pin := regexp.MustCompile(`SETUP PIN:\s+(\S+)`).FindStringSubmatch(out)
	if key == nil || pin == nil {
		t.Fatalf("key or PIN missing from output:\n%s", out)
	}

	admin := newClient(t, base)
	var enr struct{ Secret string }
	admin.post(t, "/api/setup/pin", map[string]string{"pin": pin[1]}, 200, &enr)
	admin.post(t, "/api/setup/confirm", nil, 200, nil)

	// The admin signs in, unlocks admin tools and adds alice.
	admin.signIn(t, enr.Secret, "admin")
	admin.post(t, "/api/admin/login", map[string]string{"password": "correct horse"}, 200, nil)
	var created struct{ Enrollment struct{ Secret string } }
	admin.post(t, "/api/admin/users", map[string]string{"name": "alice"}, 201, &created)

	alice := newClient(t, base)
	alice.signIn(t, created.Enrollment.Secret, "alice")
	alice.post(t, "/api/messages", map[string]string{"text": "survives restarts"}, 201, nil)
	p.kill()

	raw, err := os.ReadFile(db)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("survives restarts")) || bytes.HasPrefix(raw, []byte("SQLite format 3")) {
		t.Fatal("database is not encrypted on disk")
	}

	// A wrong key is refused.
	bad := exec.Command(bin, append([]string{"--key", strings.Repeat("A", 55)}, common...)...)
	if out, err := bad.CombinedOutput(); err == nil || !strings.Contains(string(out), "invalid key") {
		t.Fatalf("wrong key accepted: %v\n%s", err, out)
	}

	// Restart with the key: no setup, same users and secrets, message kept.
	p = startBinary(t, bin, append([]string{"--key", key[1]}, common...)...)
	if out := p.waitFor(t, "listening on"); strings.Contains(out, "SETUP PIN") {
		t.Fatal("restart must not ask for setup again")
	}
	mustAccept(t, port)
	alice = newClient(t, base)
	alice.signIn(t, created.Enrollment.Secret, "alice")
	var list struct {
		Messages []struct{ Author, Body string }
	}
	alice.get(t, "/api/messages", &list)
	var kept []string
	for _, m := range list.Messages {
		if m.Author == "alice" {
			kept = append(kept, m.Body)
		}
	}
	if len(kept) != 1 || kept[0] != "survives restarts" {
		t.Fatalf("messages after restart: %+v", list.Messages)
	}
	p.kill()
}

// mustAccept fails unless the port accepts a connection right away, with
// no retry: once cbshare prints "listening on", clients may connect.
func mustAccept(t *testing.T, port int) {
	t.Helper()
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 2*time.Second)
	if err != nil {
		t.Fatalf("\"listening on\" was printed before the port was open: %v", err)
	}
	c.Close()
}

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// proc is a running cbshare process whose console output is collected.
type proc struct {
	cmd *exec.Cmd
	mu  sync.Mutex
	out strings.Builder
}

func startBinary(t *testing.T, bin string, args ...string) *proc {
	t.Helper()
	p := &proc{cmd: exec.Command(bin, args...)}
	stdout, err := p.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	p.cmd.Stderr = p.cmd.Stdout
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.kill)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			p.mu.Lock()
			p.out.WriteString(sc.Text() + "\n")
			p.mu.Unlock()
		}
	}()
	return p
}

func (p *proc) waitFor(t *testing.T, s string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		out := p.out.String()
		p.mu.Unlock()
		if strings.Contains(out, s) {
			return out
		}
		if p.cmd.ProcessState != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	t.Fatalf("%q not seen in output:\n%s", s, p.out.String())
	return ""
}

func (p *proc) kill() {
	if p.cmd.Process != nil && p.cmd.ProcessState == nil {
		p.cmd.Process.Kill()
		p.cmd.Wait()
	}
}

// client is a minimal browser stand-in: cookies, anti-CSRF token, and
// acceptance of the self-signed certificate.
type client struct {
	base string
	http *http.Client
	csrf string
}

func newClient(t *testing.T, base string) *client {
	jar, _ := cookiejar.New(nil)
	return &client{base: base, http: &http.Client{
		Jar:       jar,
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}}
}

func (c *client) post(t *testing.T, path string, body any, status int, out any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest("POST", c.base+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.do(t, req, status, out)
}

func (c *client) get(t *testing.T, path string, out any) {
	t.Helper()
	req, _ := http.NewRequest("GET", c.base+path, nil)
	c.do(t, req, 200, out)
}

func (c *client) do(t *testing.T, req *http.Request, status int, out any) {
	t.Helper()
	req.Header.Set("X-CSRF-Token", c.csrf)
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != status {
		t.Fatalf("%s %s: HTTP %d, want %d: %s", req.Method, req.URL.Path, resp.StatusCode, status, body)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			t.Fatal(fmt.Errorf("%s: %w", body, err))
		}
	}
}

func (c *client) signIn(t *testing.T, secret, username string) {
	t.Helper()
	code, err := auth.Code(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var r struct{ CSRF string }
	c.post(t, "/api/login", map[string]string{"username": username, "code": code}, 200, &r)
	c.csrf = r.CSRF
}
