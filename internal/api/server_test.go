// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/client"
)

type memIdempotency struct {
	mu sync.Mutex
	m  map[string]string
}

func (s *memIdempotency) IdempotentResponse(_ context.Context, key string, _ int64) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[key]
	return v, ok, nil
}

func (s *memIdempotency) PutIdempotentResponse(_ context.Context, key, resp string, _ int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = resp
	return nil
}

// socketPath returns a short path: unix socket paths are limited to 108 bytes.
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "prb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "run", "api.sock")
}

func startServer(t *testing.T, allow []uint32) (*Server, string, *atomic.Int64) {
	t.Helper()
	s := New(Options{AllowUIDs: allow, Idempotency: &memIdempotency{m: map[string]string{}}})
	counter := new(atomic.Int64)
	s.Handle("GET /v1/ok/{name}", func(w http.ResponseWriter, r *http.Request) error {
		p, _ := PeerFromContext(r.Context())
		return WriteJSON(w, http.StatusOK, map[string]any{"name": r.PathValue("name"), "uid": p.UID})
	})
	s.Handle("GET /v1/missing", func(http.ResponseWriter, *http.Request) error { return NotFound("no such thing") })
	s.Handle("GET /v1/boom", func(http.ResponseWriter, *http.Request) error { return errors.New("secret internal detail") })
	s.HandleIdempotent("POST /v1/counter", func(w http.ResponseWriter, r *http.Request) error {
		return WriteJSON(w, http.StatusCreated, map[string]int64{"n": counter.Add(1)})
	})
	path := socketPath(t)
	l, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, l) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	return s, path, counter
}

func me() []uint32 { return []uint32{uint32(os.Getuid())} }

// raw sends a raw HTTP/1.1 request like the Perl client does.
func raw(t *testing.T, path, req string) (status int, head, body string) {
	t.Helper()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprint(c, req); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var sb strings.Builder
	_ = resp.Header.Write(&sb)
	b := make([]byte, 4096)
	n, _ := resp.Body.Read(b)
	return resp.StatusCode, sb.String(), string(b[:n])
}

func TestRequestsAndErrors(t *testing.T) {
	_, path, _ := startServer(t, me())
	c := client.New(path)
	ctx := t.Context()

	var ok map[string]any
	if err := c.Do(ctx, http.MethodGet, "/v1/ok/hello", nil, &ok); err != nil {
		t.Fatal(err)
	}
	if ok["name"] != "hello" || ok["uid"] != float64(os.Getuid()) {
		t.Fatalf("response = %v", ok)
	}

	err := c.Do(ctx, http.MethodGet, "/v1/missing", nil, nil)
	if !client.IsCode(err, apiv1.CodeNotFound) || err.Error() != "no such thing" {
		t.Fatalf("not found = %v", err)
	}
	err = c.Do(ctx, http.MethodGet, "/v1/boom", nil, nil)
	if !client.IsCode(err, apiv1.CodeInternal) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("internal error leaked details or wrong code: %v", err)
	}
	if err := c.Do(ctx, http.MethodGet, "/v1/nope", nil, nil); !client.IsCode(err, apiv1.CodeNotFound) {
		t.Fatalf("unknown endpoint = %v", err)
	}
	if err := c.Do(ctx, http.MethodDelete, "/v1/ok/x", nil, nil); err == nil {
		t.Fatal("wrong method accepted")
	}

	status, head, body := raw(t, path, "GET /v1/ok/perl HTTP/1.1\r\nHost: x\r\nConnection: close\r\nX-Client-API: 1\r\n\r\n")
	if status != 200 || !strings.Contains(head, "Content-Length: ") || !strings.Contains(body, `"perl"`) {
		t.Fatalf("raw request: %d %q %q", status, head, body)
	}
	status, head, body = raw(t, path, "GET /v1/missing HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	if status != 404 || !strings.Contains(head, "Content-Length: ") || !strings.Contains(body, `"not_found"`) {
		t.Fatalf("raw error: %d %q %q", status, head, body)
	}
	status, _, body = raw(t, path, "GET /v1/ok/x HTTP/1.1\r\nHost: x\r\nConnection: close\r\nX-Client-API: 2\r\n\r\n")
	if status != 400 || !strings.Contains(body, apiv1.CodeIncompatibleVersion) {
		t.Fatalf("incompatible client accepted: %d %s", status, body)
	}
}

func TestPeerUIDPolicy(t *testing.T) {
	_, path, _ := startServer(t, []uint32{4294967294})
	err := client.New(path).Do(t.Context(), http.MethodGet, "/v1/ok/x", nil, nil)
	if !client.IsCode(err, apiv1.CodePermissionDenied) {
		t.Fatalf("foreign uid allowed: %v", err)
	}
}

func TestIdempotentReplay(t *testing.T) {
	_, path, counter := startServer(t, me())
	post := func(key string) (int, string) {
		req := "POST /v1/counter HTTP/1.1\r\nHost: x\r\nConnection: close\r\nContent-Length: 0\r\n"
		if key != "" {
			req += "Idempotency-Key: " + key + "\r\n"
		}
		status, _, body := raw(t, path, req+"\r\n")
		return status, body
	}
	s1, b1 := post("k1")
	s2, b2 := post("k1")
	if s1 != 201 || s2 != 201 || b1 != b2 || counter.Load() != 1 {
		t.Fatalf("replay: %d %s / %d %s, handler ran %d times", s1, b1, s2, b2, counter.Load())
	}
	post("k2")
	post("")
	post("")
	if counter.Load() != 4 {
		t.Fatalf("handler ran %d times, want 4", counter.Load())
	}
}

func TestEventsStream(t *testing.T) {
	s, path, _ := startServer(t, me())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	events, errc := client.New(path).Events(ctx)

	// Wait until the subscription is registered before publishing.
	for deadline := time.Now().Add(5 * time.Second); ; {
		s.events.mu.Lock()
		n := len(s.events.subs)
		s.events.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("subscription not registered")
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.Events().Publish("job.updated", map[string]any{"id": 7, "state": "uploading"})
	s.Events().Publish("backup.added", map[string]any{"volname": "backup/x"})
	for _, want := range []string{"job.updated", "backup.added"} {
		select {
		case ev := <-events:
			if ev.Type != want || ev.ID == 0 {
				t.Fatalf("event = %+v, want %s", ev, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no %s event", want)
		}
	}
	cancel()
	if err := <-errc; err != nil {
		t.Fatalf("stream error: %v", err)
	}
}

func TestBrokerDropsSlowSubscribers(t *testing.T) {
	b := NewBroker(2)
	slow, _ := b.Subscribe()
	fast, unsubscribe := b.Subscribe()
	defer unsubscribe()
	for i := range 3 {
		b.Publish("tick", i)
		<-fast
	}
	n := 0
	for range slow {
		n++
	}
	if n != 2 {
		t.Fatalf("slow subscriber got %d buffered events before being dropped", n)
	}
	b.Close()
	if _, ok := <-fast; ok {
		t.Fatal("Close did not end subscriptions")
	}
	if ch, _ := b.Subscribe(); ch != nil {
		if _, ok := <-ch; ok {
			t.Fatal("subscription after Close is open")
		}
	}
}

func TestListen(t *testing.T) {
	path := socketPath(t)
	l, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Dir(path)); fi.Mode().Perm() != 0o700 {
		t.Fatalf("socket dir mode %v", fi.Mode().Perm())
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	if _, err := Listen(path); err == nil || !strings.Contains(err.Error(), "already listening") {
		t.Fatalf("second daemon on a live socket: %v", err)
	}
	_ = l.Close()

	// A crashed daemon leaves a socket file nobody listens on.
	ul, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err == nil {
		ul.SetUnlinkOnClose(false)
		_ = ul.Close()
	}
	l2, err := Listen(path)
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	_ = l2.Close()

	file := filepath.Join(filepath.Dir(path), "file")
	_ = os.WriteFile(file, nil, 0o600)
	if _, err := Listen(file); err == nil {
		t.Fatal("regular file replaced by socket")
	}
}
