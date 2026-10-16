// SPDX-License-Identifier: AGPL-3.0-or-later

package transport

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"

	onedriveapi "github.com/rclone/rclone/backend/onedrive/api"
	"github.com/rclone/rclone/fs"
)

// Providers such as OneDrive move deleted files to a recycle bin, where
// they keep counting against the quota for weeks. Backups already wait out
// the storage's own grace period before they are deleted, so by default a
// target deletes permanently where the provider allows it: deletions go
// through a twin of its Fs opened with the backend's permanent-delete
// option. Everything else keeps using the target's own Fs, which has the
// option turned off, whatever the remote's configuration says.

// purger holds a target's permanent-delete twin.
type purger struct {
	open func(ctx context.Context) (fs.Fs, error)
	keep atomic.Bool // deletions go to the recycle bin

	mu      sync.Mutex
	twin    fs.Fs
	refused bool // the provider refused a permanent deletion
}

// deletionSpecs returns remote ("name:path") with the backend's
// permanent-delete option set to false (recycle) and to true (permanent),
// if the backend of the configured remote name has one. Both fix the
// backend's location options at their current values: the twin opens on
// first use, and must delete from the drive and folder the target opened
// even if the remote has been reconnected to another one since.
func deletionSpecs(remote string) (recycle, permanent string, ok bool) {
	name, p, ok := strings.Cut(remote, ":")
	if !ok || !remoteNameRe.MatchString(name) {
		return "", "", false
	}
	typ, err := RemoteType(name)
	if err != nil {
		return "", "", false
	}
	prof, err := ProfileFor(typ)
	if err != nil || prof.PermanentDeleteOption == "" {
		return "", "", false
	}
	ri, err := fs.Find(typ)
	if err != nil {
		return "", "", false
	}
	// The values fs.NewFs would use: from the environment, the remote's
	// section or the defaults.
	m := fs.ConfigMap(ri.Prefix, ri.Options, name, nil)
	pinned := name
	for _, key := range prof.LocationOptions {
		v, _ := m.Get(key)
		pinned += "," + key + "=" + connectionValue(v)
	}
	pinned += "," + prof.PermanentDeleteOption + "="
	return pinned + "false:" + p, pinned + "true:" + p, true
}

// connectionValue quotes v for an rclone connection string if it needs it.
func connectionValue(v string) string {
	if !strings.ContainsAny(v, `,:"'`) {
		return v
	}
	return `"` + strings.ReplaceAll(v, `"`, `""`) + `"`
}

// SetRecycleBin chooses whether objects deleted through t go to the
// provider's recycle bin (keep) or are deleted permanently, the default
// where the provider supports it. It takes effect with the next deletion.
func (t *Target) SetRecycleBin(keep bool) {
	if t.purge != nil {
		t.purge.keep.Store(keep)
	}
}

// deleteVia runs del with the Fs a deletion should use: the
// permanent-delete twin, or t.f where deletions go to the recycle bin. If
// the provider refuses to delete permanently, the deletion is made with
// t.f instead, and so are all later ones. t.f of a target with a twin
// always uses the recycle bin (see deletionSpecs).
func (t *Target) deleteVia(ctx context.Context, del func(fs.Fs) error) error {
	twin, err := t.purgeFs(ctx)
	if err != nil {
		return err
	}
	if twin == nil {
		return del(t.f)
	}
	refusal := del(twin)
	if !permanentDeleteRefused(refusal) {
		return refusal
	}
	// A failure here is not about permanent deletion; the next deletion
	// tries the twin again.
	if err := del(t.f); err != nil {
		return err
	}
	t.purge.mu.Lock()
	first := !t.purge.refused
	t.purge.refused = true
	t.purge.mu.Unlock()
	if first {
		fs.Logf(t.f, "The provider refused to delete permanently (%v): deleted files go to its recycle bin "+
			"and count against the quota until the recycle bin is emptied", refusal)
	}
	return nil
}

// purgeFs returns the permanent-delete twin, opening it on first use, or
// nil if deletions use t.f.
func (t *Target) purgeFs(ctx context.Context) (fs.Fs, error) {
	p := t.purge
	if p == nil || p.keep.Load() {
		return nil, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.refused {
		return nil, nil
	}
	if p.twin == nil {
		f, err := p.open(ctx)
		if err != nil {
			return nil, err
		}
		p.twin = f
	}
	return p.twin, nil
}

// permanentDeleteRefused reports whether err is a provider's refusal of a
// permanent deletion: an answer that says the request is not allowed or
// not supported, rather than that the item is missing, the remote is
// throttled or full.
func permanentDeleteRefused(err error) bool {
	ae, ok := errors.AsType[*onedriveapi.Error](err)
	if !ok {
		return false
	}
	switch code := ae.ErrorInfo.Code; code {
	case "accessDenied", "notAllowed", "notSupported", "invalidRequest", "methodNotAllowed":
		return true
	default:
		// rclone reports the HTTP status as the code of an answer without
		// an error document.
		for _, status := range []string{"400 ", "403 ", "405 ", "501 "} {
			if strings.HasPrefix(code, status) {
				return true
			}
		}
	}
	return false
}
