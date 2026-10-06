// SPDX-License-Identifier: AGPL-3.0-or-later

// Package faultfs registers the rclone backend "faulty", which wraps
// another remote and injects failures: errors on upload, corrupted stored
// bytes, wrong provider hashes and objects missing from listings. It is
// for tests only and must not be imported by the daemon.
//
// Use it through a connection string:
//
//	faultfs.Register("t1", ctl)
//	fs.NewFs(ctx, ":faulty,id=t1,remote='/tmp/x':")
package faultfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/fspath"
	"github.com/rclone/rclone/fs/hash"
)

// Controller decides which failures to inject. All hooks are optional and
// receive the object path relative to the faulty remote's root.
type Controller struct {
	// PutError is called before each upload with the attempt number for
	// that path (starting at 1); a non-nil error fails the upload.
	PutError func(remote string, attempt int) error
	// Corrupt flips one stored byte of uploads of matching paths.
	Corrupt func(remote string) bool
	// WrongHash makes the provider report a wrong hash for matching paths.
	WrongHash func(remote string) bool
	// Hide removes matching paths from listings and lookups.
	Hide func(remote string) bool
	// VanishAfterList deletes matching objects right after they are
	// listed (still returning them), as a concurrent deletion would.
	VanishAfterList func(remote string) bool

	mu   sync.Mutex
	puts map[string]int
}

// Puts returns how often an upload of remote was attempted.
func (c *Controller) Puts(remote string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.puts[remote]
}

// TotalPuts returns the number of upload attempts across all paths. Paths
// seen by the faulty layer are encrypted when it sits below crypt.
func (c *Controller) TotalPuts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.puts {
		n += v
	}
	return n
}

func (c *Controller) nextPut(remote string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.puts == nil {
		c.puts = map[string]int{}
	}
	c.puts[remote]++
	return c.puts[remote]
}

var controllers sync.Map

// Register makes a controller available under id.
func Register(id string, c *Controller) { controllers.Store(id, c) }

// rclone logs "no overview data found" for backends outside its own tree;
// that message is expected in tests.
func init() {
	fs.Register(&fs.RegInfo{
		Name:        "faulty",
		Description: "Fault injection wrapper (tests only)",
		NewFs:       newFs,
		Options: []fs.Option{
			{Name: "remote", Help: "Remote to wrap.", Required: true},
			{Name: "id", Help: "Controller id.", Required: true},
		},
	})
}

// Fs wraps another Fs.
type Fs struct {
	name     string
	root     string
	wrapped  fs.Fs
	ctl      *Controller
	features *fs.Features
}

func newFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	remote, _ := m.Get("remote")
	id, _ := m.Get("id")
	v, ok := controllers.Load(id)
	if !ok {
		return nil, fmt.Errorf("faulty: no controller registered as %q", id)
	}
	wrapped, err := fs.NewFs(ctx, fspath.JoinRootPath(remote, root))
	if err != nil && !errors.Is(err, fs.ErrorIsFile) {
		return nil, err
	}
	f := &Fs{name: name, root: root, wrapped: wrapped, ctl: v.(*Controller)}
	f.features = (&fs.Features{CanHaveEmptyDirectories: true}).Fill(ctx, f).Mask(ctx, wrapped).WrapsFs(f, wrapped)
	return f, err
}

func (f *Fs) Name() string              { return f.name }
func (f *Fs) Root() string              { return f.root }
func (f *Fs) String() string            { return "faulty:" + f.wrapped.String() }
func (f *Fs) Precision() time.Duration  { return f.wrapped.Precision() }
func (f *Fs) Hashes() hash.Set          { return f.wrapped.Hashes() }
func (f *Fs) Features() *fs.Features    { return f.features }
func (f *Fs) UnWrap() fs.Fs             { return f.wrapped }
func (f *Fs) hidden(remote string) bool { return f.ctl.Hide != nil && f.ctl.Hide(remote) }

func (f *Fs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	entries, err := f.wrapped.List(ctx, dir)
	if err != nil {
		return nil, err
	}
	out := entries[:0]
	for _, e := range entries {
		if f.hidden(e.Remote()) {
			continue
		}
		if o, ok := e.(fs.Object); ok {
			if f.ctl.VanishAfterList != nil && f.ctl.VanishAfterList(o.Remote()) {
				if err := o.Remove(ctx); err != nil {
					return nil, err
				}
			}
			out = append(out, &Object{Object: o, f: f})
		} else {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	if f.hidden(remote) {
		return nil, fs.ErrorObjectNotFound
	}
	o, err := f.wrapped.NewObject(ctx, remote)
	if err != nil {
		return nil, err
	}
	return &Object{Object: o, f: f}, nil
}

func (f *Fs) wrapReader(remote string, in io.Reader) io.Reader {
	if f.ctl.Corrupt != nil && f.ctl.Corrupt(remote) {
		return &flipReader{r: in, at: 40}
	}
	return in
}

func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	remote := src.Remote()
	attempt := f.ctl.nextPut(remote)
	if f.ctl.PutError != nil {
		if err := f.ctl.PutError(remote, attempt); err != nil {
			_, _ = io.Copy(io.Discard, in)
			return nil, err
		}
	}
	o, err := f.wrapped.Put(ctx, f.wrapReader(remote, in), src, options...)
	if err != nil {
		return nil, err
	}
	return &Object{Object: o, f: f}, nil
}

func (f *Fs) Mkdir(ctx context.Context, dir string) error { return f.wrapped.Mkdir(ctx, dir) }
func (f *Fs) Rmdir(ctx context.Context, dir string) error { return f.wrapped.Rmdir(ctx, dir) }

// Object wraps an object of the wrapped Fs.
type Object struct {
	fs.Object
	f *Fs
}

func (o *Object) Fs() fs.Info       { return o.f }
func (o *Object) UnWrap() fs.Object { return o.Object }

func (o *Object) Hash(ctx context.Context, t hash.Type) (string, error) {
	h, err := o.Object.Hash(ctx, t)
	if err == nil && h != "" && o.f.ctl.WrongHash != nil && o.f.ctl.WrongHash(o.Remote()) {
		return "0000" + h[4:], nil
	}
	return h, err
}

func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	return o.Object.Update(ctx, o.f.wrapReader(o.Remote(), in), src, options...)
}

// flipReader inverts the byte at offset at.
type flipReader struct {
	r   io.Reader
	at  int64
	pos int64
}

func (fr *flipReader) Read(p []byte) (int, error) {
	n, err := fr.r.Read(p)
	if fr.at >= fr.pos && fr.at < fr.pos+int64(n) {
		p[fr.at-fr.pos] ^= 0xff
	}
	fr.pos += int64(n)
	return n, err
}
