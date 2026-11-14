package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestSecurityHeaders(t *testing.T) {
	h := newHarness(t, nil)
	c := h.client()
	for _, path := range []string{"/", "/app.js", "/api/state", "/missing"} {
		r := c.do("GET", path, nil)
		csp := r.header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s: CSP %q", path, csp)
		}
		for name, want := range map[string]string{
			"X-Content-Type-Options":    "nosniff",
			"X-Frame-Options":           "DENY",
			"Referrer-Policy":           "no-referrer",
			"Strict-Transport-Security": "max-age=86400",
		} {
			if got := r.header.Get(name); got != want {
				t.Errorf("%s: %s = %q, want %q", path, name, got, want)
			}
		}
	}
}

func TestCSRFTokenRequired(t *testing.T) {
	h := newHarness(t, nil)
	h.setup()
	c := h.login("alice")
	token := c.csrf

	c.csrf = ""
	r := c.expect(c.do("POST", "/api/messages", map[string]string{"text": "x"}), 403)
	if r.body["code"] != "csrf" {
		t.Fatalf("got %v", r.body)
	}
	c.csrf = "forged"
	c.expect(c.do("POST", "/api/messages", map[string]string{"text": "x"}), 403)
	c.csrf = ""
	c.expect(c.do("GET", "/api/messages", nil), 200) // reads need no token
	c.csrf = token
	c.expect(c.do("POST", "/api/messages", map[string]string{"text": "x"}), 201)
}

func TestCrossOriginRejected(t *testing.T) {
	h := newHarness(t, nil)
	h.setup()
	c := h.login("alice")

	req, _ := http.NewRequest("POST", h.ts.URL+"/api/messages", strings.NewReader(`{"text":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", c.csrf)
	req.Header.Set("Origin", "https://evil.example")
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("cross-origin POST: HTTP %d", resp.StatusCode)
	}

	req, _ = http.NewRequest("POST", h.ts.URL+"/api/login", strings.NewReader(`{"code":"123456"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err = c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("cross-site login: HTTP %d", resp.StatusCode)
	}
}

func TestJSONContentTypeRequired(t *testing.T) {
	h := newHarness(t, nil)
	c := h.client()
	req, _ := http.NewRequest("POST", h.ts.URL+"/api/setup/pin", strings.NewReader(`{"pin":"x"}`))
	req.Header.Set("Content-Type", "text/plain") // what a cross-site form could send
	r := c.send(req)
	if r.status != http.StatusUnsupportedMediaType {
		t.Fatalf("HTTP %d", r.status)
	}
}
