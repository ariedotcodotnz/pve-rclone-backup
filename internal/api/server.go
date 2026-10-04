// SPDX-License-Identifier: AGPL-3.0-or-later

// Package api serves the daemon API (HTTP/1.1 + JSON) on a unix socket.
//
// Only peers whose SO_PEERCRED uid is allowed (root by default) may talk to
// it. Non-streaming responses always carry Content-Length so minimal
// clients, like the Perl storage plugin, can read them without chunked
// decoding.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
)

// Error is an API error with an HTTP status.
type Error struct {
	Status int
	Body   apiv1.Error
}

func (e *Error) Error() string { return e.Body.Code + ": " + e.Body.Message }

// Errorf returns an *Error.
func Errorf(status int, code, format string, args ...any) *Error {
	return &Error{Status: status, Body: apiv1.Error{Code: code, Message: fmt.Sprintf(format, args...)}}
}

// NotFound returns a not_found error.
func NotFound(format string, args ...any) *Error {
	return Errorf(http.StatusNotFound, apiv1.CodeNotFound, format, args...)
}

// Invalid returns an invalid_argument error.
func Invalid(format string, args ...any) *Error {
	return Errorf(http.StatusBadRequest, apiv1.CodeInvalidArgument, format, args...)
}

// HandlerFunc handles a request. A returned *Error is sent to the client;
// any other error becomes an internal error and is logged.
type HandlerFunc func(w http.ResponseWriter, r *http.Request) error

// IdempotencyStore persists responses of mutating requests.
type IdempotencyStore interface {
	IdempotentResponse(ctx context.Context, key string, maxAgeSeconds int64) (string, bool, error)
	PutIdempotentResponse(ctx context.Context, key, responseJSON string, maxAgeSeconds int64) error
}

// Options configures a Server.
type Options struct {
	Logger *slog.Logger
	// AllowUIDs lists the peer uids allowed to use the API (default: root).
	AllowUIDs []uint32
	// Idempotency stores replies for requests with an Idempotency-Key.
	Idempotency IdempotencyStore
}

// Server is the API server.
type Server struct {
	log         *slog.Logger
	allow       []uint32
	idempotency IdempotencyStore
	mux         *http.ServeMux
	events      *Broker
}

// New returns a server with no routes registered.
func New(opts Options) *Server {
	s := &Server{
		log:         opts.Logger,
		allow:       opts.AllowUIDs,
		idempotency: opts.Idempotency,
		mux:         http.NewServeMux(),
		events:      NewBroker(256),
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if len(s.allow) == 0 {
		s.allow = []uint32{0}
	}
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.writeError(w, r, NotFound("no such endpoint: %s %s", r.Method, r.URL.Path))
	})
	s.Handle("GET /v1/events", s.handleEvents)
	return s
}

// Events returns the event broker.
func (s *Server) Events() *Broker { return s.events }

// Handle registers a handler for a method-aware ServeMux pattern such as
// "GET /v1/storages/{storage}".
func (s *Server) Handle(pattern string, h HandlerFunc) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			s.writeError(w, r, err)
		}
	})
}

// HandleIdempotent registers a mutating handler whose successful responses
// are replayed for requests repeating an Idempotency-Key within 24 hours.
func (s *Server) HandleIdempotent(pattern string, h HandlerFunc) {
	s.Handle(pattern, s.idempotent(h))
}

type ctxKey int

const (
	connKey ctxKey = iota
	peerKey
)

// Peer identifies the process on the other end of a connection.
type Peer struct {
	PID int32
	UID uint32
	GID uint32
}

// PeerFromContext returns the peer of a request.
func PeerFromContext(ctx context.Context) (Peer, bool) {
	p, ok := ctx.Value(peerKey).(Peer)
	return p, ok
}

func peerCred(c net.Conn) (Peer, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return Peer{}, errors.New("not a unix socket connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return Peer{}, err
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return Peer{}, err
	}
	if credErr != nil {
		return Peer{}, credErr
	}
	return Peer{PID: cred.Pid, UID: cred.Uid, GID: cred.Gid}, nil
}

// ServeHTTP authenticates the peer, checks the client API revision and
// dispatches the request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	defer func() {
		s.log.Debug("api request", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"duration", time.Since(start).Round(time.Microsecond))
	}()

	c, _ := r.Context().Value(connKey).(net.Conn)
	peer, err := peerCred(c)
	if err != nil {
		s.writeError(rec, r, Errorf(http.StatusForbidden, apiv1.CodePermissionDenied, "cannot identify peer: %v", err))
		return
	}
	if !slices.Contains(s.allow, peer.UID) {
		s.log.Warn("api request from unauthorized peer refused", "uid", peer.UID, "pid", peer.PID)
		s.writeError(rec, r, Errorf(http.StatusForbidden, apiv1.CodePermissionDenied, "uid %d may not use this API", peer.UID))
		return
	}
	if v := r.Header.Get(apiv1.ClientAPIHeader); v != "" {
		if n, err := strconv.Atoi(v); err != nil || n != apiv1.Revision {
			s.writeError(rec, r, Errorf(http.StatusBadRequest, apiv1.CodeIncompatibleVersion,
				"client speaks API revision %q but this daemon serves revision %d; update pve-rclone-backup on this node", v, apiv1.Revision))
			return
		}
	}
	s.mux.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), peerKey, peer)))
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// WriteJSON writes v as JSON with an explicit Content-Length.
func WriteJSON(w http.ResponseWriter, status int, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, err = w.Write(body)
	return err
}

// DecodeJSON decodes a request body into v, rejecting unknown fields and
// bodies over 1 MiB.
func DecodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return Invalid("invalid request body: %v", err)
	}
	return nil
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		s.log.Error("api handler failed", "method", r.Method, "path", r.URL.Path, "err", err)
		apiErr = Errorf(http.StatusInternalServerError, apiv1.CodeInternal, "internal error; see the daemon log")
	}
	if werr := WriteJSON(w, apiErr.Status, apiv1.ErrorResponse{Error: apiErr.Body}); werr != nil {
		s.log.Debug("write error response", "err", werr)
	}
}

const idempotencyTTL = 24 * 60 * 60

type storedResponse struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

type captureWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (c *captureWriter) Header() http.Header         { return c.header }
func (c *captureWriter) Write(p []byte) (int, error) { return c.body.Write(p) }
func (c *captureWriter) WriteHeader(code int)        { c.status = code }

func (s *Server) idempotent(h HandlerFunc) HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		key := r.Header.Get(apiv1.IdempotencyHeader)
		if key == "" || s.idempotency == nil {
			return h(w, r)
		}
		peer, _ := PeerFromContext(r.Context())
		scoped := fmt.Sprintf("%d %s %s %s", peer.UID, r.Method, r.URL.Path, key)
		if raw, ok, err := s.idempotency.IdempotentResponse(r.Context(), scoped, idempotencyTTL); err != nil {
			return err
		} else if ok {
			var sr storedResponse
			if err := json.Unmarshal([]byte(raw), &sr); err != nil {
				return err
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", strconv.Itoa(len(sr.Body)))
			w.Header().Set("Idempotent-Replayed", "true")
			w.WriteHeader(sr.Status)
			_, err := w.Write(sr.Body)
			return err
		}
		cw := &captureWriter{header: w.Header(), status: http.StatusOK}
		err := h(cw, r)
		if err == nil && cw.status < 300 {
			body := cw.body.Bytes()
			if !json.Valid(body) {
				body = []byte("null")
			}
			raw, _ := json.Marshal(storedResponse{Status: cw.status, Body: body})
			if perr := s.idempotency.PutIdempotentResponse(r.Context(), scoped, string(raw), idempotencyTTL); perr != nil {
				s.log.Warn("store idempotent response", "err", perr)
			}
		}
		if err != nil {
			return err
		}
		w.WriteHeader(cw.status)
		_, werr := w.Write(cw.body.Bytes())
		return werr
	}
}

// Listen creates the unix socket at path with mode 0600 inside a 0700
// directory. A stale socket left by a crashed daemon is replaced; a socket
// that still accepts connections means another daemon is running.
func Listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("api: create socket directory: %w", err)
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("api: %s exists and is not a socket", path)
		}
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			_ = c.Close()
			return nil, fmt.Errorf("api: another daemon is already listening on %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("api: remove stale socket: %w", err)
		}
	}
	old := unix.Umask(0o177)
	l, err := net.Listen("unix", path)
	unix.Umask(old)
	if err != nil {
		return nil, fmt.Errorf("api: listen on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("api: chmod socket: %w", err)
	}
	return l, nil
}

// Serve serves requests on l until ctx is cancelled, then shuts down
// gracefully, ending open event streams.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	srv := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, connKey, c)
		},
		ErrorLog: slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
	}
	srv.RegisterOnShutdown(s.events.Close)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(l) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("api: shutdown: %w", err)
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
