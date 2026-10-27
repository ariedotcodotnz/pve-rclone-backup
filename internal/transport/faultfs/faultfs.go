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
//
// Like OneDrive, it has an option hard_delete for deletions that bypass a
// recycle bin; the controller records which kind each deletion was. Like
// Google Drive, with the option duplicate_files it reports that several
// objects may share a name; the controller records uploads that would have
// added another object of an existing name.
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
	// ListError is called before each listing with the attempt number for
	// that directory (starting at 1); a non-nil error fails the listing.
	ListError func(dir string, attempt int) error
	// HardDeleteError is called before each permanent deletion of an
	// object or directory; a non-nil error fails the deletion.
	HardDeleteError func(remote string) error

	mu         sync.Mutex
	puts       map[string]int
	lists      map[string]int
	flushes    int
	deletions  []string
	duplicates []string
}

// Duplicates lists the uploads that created a second object of an existing
// name, on a remote with duplicate_files.
func (c *Controller) Duplicates() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.duplicates...)
}

// Deletions lists the objects and directories deleted so far, in order,
// as "permanent:<path>" or "recycled:<path>".
func (c *Controller) Deletions() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.deletions...)
}

func (c *Controller) deleted(remote string, permanent bool) {
	kind := "recycled:"
	if permanent {
		kind = "permanent:"
	}
	c.mu.Lock()
	c.deletions = append(c.deletions, kind+remote)
	c.mu.Unlock()
}

// Flushes returns how often the directory cache was flushed.
func (c *Controller) Flushes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.flushes
}

func (c *Controller) nextList(dir string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lists == nil {
		c.lists = map[string]int{}
	}
	c.lists[dir]++
	return c.lists[dir]
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
			{Name: "hard_delete", Help: "Delete permanently instead of using the recycle bin.", Default: false},
			{Name: "duplicate_files", Help: "Report that several objects may share a name.", Default: false},
		},
	})
}

// Fs wraps another Fs.
type Fs struct {
	name       string
	root       string
	wrapped    fs.Fs
	ctl        *Controller
	features   *fs.Features
	hardDelete bool
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
	hard, _ := m.Get("hard_delete")
	dup, _ := m.Get("duplicate_files")
	f := &Fs{name: name, root: root, wrapped: wrapped, ctl: v.(*Controller), hardDelete: hard == "true"}
	f.features = (&fs.Features{CanHaveEmptyDirectories: true}).Fill(ctx, f).Mask(ctx, wrapped).WrapsFs(f, wrapped)
	f.features.DuplicateFiles = dup == "true"
	// Like the backends that cache directory IDs, even over one that does not.
	f.features.DirCacheFlush = f.DirCacheFlush
	return f, err
}

// DirCacheFlush records the flush and passes it on.
func (f *Fs) DirCacheFlush() {
	f.ctl.mu.Lock()
	f.ctl.flushes++
	f.ctl.mu.Unlock()
	if flush := f.wrapped.Features().DirCacheFlush; flush != nil {
		flush()
	}
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
	if f.ctl.ListError != nil {
		if err := f.ctl.ListError(dir, f.ctl.nextList(dir)); err != nil {
			return nil, err
		}
	}
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
	if f.features.DuplicateFiles {
		if _, err := f.wrapped.NewObject(ctx, remote); err == nil {
			f.ctl.mu.Lock()
			f.ctl.duplicates = append(f.ctl.duplicates, remote)
			f.ctl.mu.Unlock()
		}
	}
	o, err := f.wrapped.Put(ctx, f.wrapReader(remote, in), src, options...)
	if err != nil {
		return nil, err
	}
	return &Object{Object: o, f: f}, nil
}

func (f *Fs) Mkdir(ctx context.Context, dir string) error { return f.wrapped.Mkdir(ctx, dir) }
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	return f.remove(dir, func() error { return f.wrapped.Rmdir(ctx, dir) })
}

// remove runs a deletion of remote and records it.
func (f *Fs) remove(remote string, del func() error) error {
	if f.hardDelete && f.ctl.HardDeleteError != nil {
		if err := f.ctl.HardDeleteError(remote); err != nil {
			return err
		}
	}
	if err := del(); err != nil {
		return err
	}
	f.ctl.deleted(remote, f.hardDelete)
	return nil
}

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

func (o *Object) Remove(ctx context.Context) error {
	return o.f.remove(o.Remote(), func() error { return o.Object.Remove(ctx) })
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
