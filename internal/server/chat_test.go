package server

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/pycloux/cpsync/internal/config"
)

func (c *client) upload(name string, data []byte, caption string) result {
	c.h.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if caption != "" {
		mw.WriteField("caption", caption)
	}
	fw, _ := mw.CreateFormFile("file", name)
	fw.Write(data)
	mw.Close()
	req, _ := http.NewRequest("POST", c.h.ts.URL+"/api/files", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return c.send(req)
}

func messageList(r result) []map[string]any {
	var out []map[string]any
	if list, ok := r.body["messages"].([]any); ok {
		for _, m := range list {
			out = append(out, m.(map[string]any))
		}
	}
	return out
}

func TestChatFlow(t *testing.T) {
	h := newHarness(t, nil)
	h.setup()
	alice := h.login("alice")
	bob := h.login("bob")

	script := "#!/bin/sh\r\nset -e\n\techo \"hello\"  \n"
	r := alice.expect(alice.do("POST", "/api/messages", map[string]string{"text": script}), 201)
	id := int(r.body["id"].(float64))
	if r.body["body"] != strings.ReplaceAll(script, "\r\n", "\n") || r.body["author"] != "alice" {
		t.Fatalf("text must be kept exactly: %q", r.body["body"])
	}
	alice.expect(alice.do("POST", "/api/messages", map[string]string{"text": "  \n "}), 400)

	// Bob sees it, but cannot change it.
	r = bob.expect(bob.do("GET", "/api/messages", nil), 200)
	if list := messageList(r); len(list) != 1 || list[0]["author"] != "alice" {
		t.Fatalf("bob's view: %v", r.body)
	}
	path := fmt.Sprintf("/api/messages/%d", id)
	bob.expect(bob.do("PUT", path, map[string]string{"text": "hacked"}), 403)
	bob.expect(bob.do("DELETE", path, nil), 403)

	// Ownership follows the user: alice's other session may edit.
	alice2 := h.login("alice")
	r = alice2.expect(alice2.do("PUT", path, map[string]string{"text": "echo edited"}), 200)
	if r.body["body"] != "echo edited" || r.body["editedAt"] == nil {
		t.Fatalf("edit: %v", r.body)
	}
	alice.expect(alice.do("DELETE", path, nil), 200)
	alice.expect(alice.do("DELETE", path, nil), 404)

	// Unauthenticated clients are refused.
	anon := h.client()
	anon.expect(anon.do("GET", "/api/messages", nil), 401)
}

func TestRevision(t *testing.T) {
	h := newHarness(t, nil)
	h.setup()
	c := h.login("alice")

	r := c.expect(c.do("GET", "/api/messages", nil), 200)
	rev := int64(r.body["rev"].(float64))
	r = c.expect(c.do("GET", fmt.Sprintf("/api/messages?since=%d", rev), nil), 200)
	if r.body["unchanged"] != true {
		t.Fatalf("expected unchanged: %v", r.body)
	}
	c.expect(c.do("POST", "/api/messages", map[string]string{"text": "x"}), 201)
	r = c.expect(c.do("GET", fmt.Sprintf("/api/messages?since=%d", rev), nil), 200)
	if r.body["unchanged"] == true || len(messageList(r)) != 1 {
		t.Fatalf("expected the new message: %v", r.body)
	}
}

func TestFiles(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.MaxFileSizeMB = 1 })
	h.setup()
	alice := h.login("alice")
	bob := h.login("bob")

	data := bytes.Repeat([]byte{0, 1, 2, 255}, 1000)
	r := alice.expect(alice.upload(`..\..\evil"name<>.bin`, data, "firmware"), 201)
	file := r.body["file"].(map[string]any)
	if file["name"] != "evil_name__.bin" || file["size"] != float64(len(data)) || r.body["body"] != "firmware" {
		t.Fatalf("upload: %v", r.body)
	}

	r = bob.expect(bob.do("GET", fmt.Sprintf("/api/files/%d", int(file["id"].(float64))), nil), 200)
	if !bytes.Equal(r.raw, data) {
		t.Fatal("downloaded content differs")
	}
	if r.header.Get("Content-Type") != "application/octet-stream" ||
		!strings.HasPrefix(r.header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("download headers: %v", r.header)
	}

	alice.expect(alice.upload("big.bin", make([]byte, 1<<20+1), ""), 413)
	alice.expect(alice.upload("ok.bin", make([]byte, 1<<20), ""), 201)

	// A file message's caption can be edited, and deleting removes the file.
	list := messageList(alice.expect(alice.do("GET", "/api/messages", nil), 200))
	first := list[0]
	fid := int(first["file"].(map[string]any)["id"].(float64))
	alice.expect(alice.do("PUT", fmt.Sprintf("/api/messages/%d", int(first["id"].(float64))), map[string]string{"text": ""}), 200)
	alice.expect(alice.do("DELETE", fmt.Sprintf("/api/messages/%d", int(first["id"].(float64))), nil), 200)
	bob.expect(bob.do("GET", fmt.Sprintf("/api/files/%d", fid), nil), 404)
}

func TestRetentionCountsSystemMessages(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.MaxMessages = 3; c.MaxFailedAttempts = 100 })
	h.setup()
	c := h.login("alice")
	for i := 0; i < 3; i++ {
		c.expect(c.do("POST", "/api/messages", map[string]string{"text": fmt.Sprint("m", i)}), 201)
	}
	h.client().do("POST", "/api/login", map[string]string{"code": "000000"}) // system message
	list := messageList(c.expect(c.do("GET", "/api/messages", nil), 200))
	if len(list) != 3 || list[0]["body"] != "m1" || list[2]["kind"] != "system" {
		t.Fatalf("retention: %v", list)
	}
}

func TestCleanFileName(t *testing.T) {
	cases := map[string]string{
		"report.pdf":                         "report.pdf",
		"/etc/passwd":                        "passwd",
		`C:\Users\a\key.pem`:                 "key.pem",
		"..":                                 "file",
		"":                                   "file",
		"a\x00b\nc.txt":                      "a_b_c.txt",
		strings.Repeat("x", 300) + ".tar.gz": strings.Repeat("x", 197) + ".gz",
	}
	for in, want := range cases {
		if got := cleanFileName(in); got != want {
			t.Errorf("cleanFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatSize(t *testing.T) {
	for n, want := range map[int64]string{20 << 20: "20 MB", 256 << 10: "256 KB", 1536 << 10: "1.5 MB", 10: "10 bytes"} {
		if got := formatSize(n); got != want {
			t.Errorf("formatSize(%d) = %q, want %q", n, got, want)
		}
	}
}
