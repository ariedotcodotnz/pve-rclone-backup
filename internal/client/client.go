// SPDX-License-Identifier: AGPL-3.0-or-later

// Package client is the Go client of the daemon API, used by the CLI and
// the TUI.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
)

// DefaultSocket is the daemon's API socket.
const DefaultSocket = "/run/pve-rclone-backup/api.sock"

// Error is an error reported by the daemon (or a failure to reach it,
// with code apiv1.CodeUnavailable).
type Error struct {
	Status int
	Body   apiv1.Error
}

func (e *Error) Error() string { return e.Body.Message }

// IsCode reports whether err is a daemon error with the given code.
func IsCode(err error, code string) bool {
	e, ok := errors.AsType[*Error](err)
	return ok && e.Body.Code == code
}

// Client talks to the daemon over its unix socket.
type Client struct {
	socket string
	http   *http.Client
	stream *http.Client
}

// New returns a client for the daemon listening on socket.
func New(socket string) *Client {
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}
	return &Client{
		socket: socket,
		http:   &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{DialContext: dial}},
		stream: &http.Client{Transport: &http.Transport{DialContext: dial}},
	}
}

const base = "http://pve-rclone-backupd"

func (c *Client) newRequest(ctx context.Context, method, path string, in any) (*http.Request, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set(apiv1.ClientAPIHeader, strconv.Itoa(apiv1.Revision))
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (c *Client) unavailable(err error) error {
	return &Error{Body: apiv1.Error{Code: apiv1.CodeUnavailable,
		Message: fmt.Sprintf("cannot reach pve-rclone-backupd at %s (is the service running?): %v", c.socket, err)}}
}

// Do performs a request and decodes the JSON response into out (if not nil).
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	req, err := c.newRequest(ctx, method, path, in)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return c.unavailable(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("read daemon response: %w", err)
	}
	if resp.StatusCode >= 400 {
		var er apiv1.ErrorResponse
		if json.Unmarshal(data, &er) == nil && er.Error.Code != "" {
			return &Error{Status: resp.StatusCode, Body: er.Error}
		}
		return &Error{Status: resp.StatusCode, Body: apiv1.Error{Code: apiv1.CodeInternal,
			Message: fmt.Sprintf("daemon returned %s", resp.Status)}}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode daemon response: %w", err)
		}
	}
	return nil
}

// Version returns the daemon's version information.
func (c *Client) Version(ctx context.Context) (*apiv1.Version, error) {
	var v apiv1.Version
	return &v, c.Do(ctx, http.MethodGet, "/v1/version", nil, &v)
}

// Status returns the daemon's health summary.
func (c *Client) Status(ctx context.Context) (*apiv1.Status, error) {
	var s apiv1.Status
	return &s, c.Do(ctx, http.MethodGet, "/v1/status", nil, &s)
}

// Events streams daemon events until ctx is cancelled or the stream ends.
// The events channel is closed when the stream ends; the error channel then
// receives the reason (nil for a normal end).
func (c *Client) Events(ctx context.Context) (<-chan apiv1.Event, <-chan error) {
	events := make(chan apiv1.Event, 64)
	errc := make(chan error, 1)
	go func() {
		defer close(events)
		errc <- c.readEvents(ctx, events)
	}()
	return events, errc
}

func (c *Client) readEvents(ctx context.Context, out chan<- apiv1.Event) error {
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.stream.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return c.unavailable(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return &Error{Status: resp.StatusCode, Body: apiv1.Error{Code: apiv1.CodeInternal, Message: "event stream refused: " + resp.Status}}
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var data strings.Builder
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if data.Len() > 0 {
				var ev apiv1.Event
				if err := json.Unmarshal([]byte(data.String()), &ev); err != nil {
					return fmt.Errorf("decode event: %w", err)
				}
				data.Reset()
				select {
				case out <- ev:
				case <-ctx.Done():
					return nil
				}
			}
		case strings.HasPrefix(line, "data: "):
			data.WriteString(strings.TrimPrefix(line, "data: "))
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return sc.Err()
}
