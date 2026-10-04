// SPDX-License-Identifier: AGPL-3.0-or-later

package transport

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/lib/atexit"
)

// Options configures the process-wide rclone engine.
type Options struct {
	// ConfigPath names the configuration store. rclone ignores a custom
	// Storage unless a non-empty config path is set first. It is used as an
	// identifier only; Storage decides where data actually lives.
	ConfigPath string

	// Storage replaces rclone's configuration storage. Nil keeps rclone's
	// in-memory default.
	//
	// Storage.Load must only ever return nil or config.ErrorConfigFileNotFound:
	// rclone treats any other load error as fatal and exits the process.
	Storage config.Storage

	// LowLevelRetries bounds rclone's per-request retries. Zero keeps the
	// rclone default.
	LowLevelRetries int

	// Logger receives rclone's log output (nil keeps rclone's default). It
	// should be the daemon's redacting handler.
	Logger slog.Handler

	// Debug enables rclone's debug logging.
	Debug bool

	// UserAgent identifies requests to providers (default
	// "pve-rclone-backup rclone/<version>").
	UserAgent string
}

var (
	initMu   sync.Mutex
	initDone bool
)

// ErrAlreadyInitialized is returned when Init is called more than once.
var ErrAlreadyInitialized = errors.New("transport: rclone engine already initialized")

// Init configures rclone's global state. It must be called once per process
// before any other function in this package.
func Init(opts Options) error {
	initMu.Lock()
	defer initMu.Unlock()
	if initDone {
		return ErrAlreadyInitialized
	}

	if opts.Storage != nil {
		if opts.ConfigPath == "" {
			return errors.New("transport: ConfigPath is required when Storage is set")
		}
		if err := config.SetConfigPath(opts.ConfigPath); err != nil {
			return fmt.Errorf("transport: set config path: %w", err)
		}
		config.SetData(opts.Storage)
	}

	ci := fs.GetConfig(context.Background())
	if opts.LowLevelRetries > 0 {
		ci.LowLevelRetries = opts.LowLevelRetries
	}
	if opts.Logger != nil {
		fs.SetLogger(opts.Logger)
	}
	ci.LogLevel = fs.LogLevelNotice
	if opts.Debug {
		ci.LogLevel = fs.LogLevelDebug
	}
	// Microsoft recommends identifying traffic to reduce throttling.
	ci.UserAgent = cmp.Or(opts.UserAgent, "pve-rclone-backup rclone/"+fs.Version)
	// Segments are uploaded as single streams; resumption happens at segment
	// granularity in our own job state, not inside rclone.
	ci.MultiThreadStreams = 0

	// rclone installs a SIGINT/SIGTERM handler that runs its exit hooks
	// and calls os.Exit as soon as anything registers one (every upload
	// does). The daemon shuts down gracefully itself and calls Shutdown.
	atexit.IgnoreSignals()

	initDone = true
	return nil
}

// Shutdown runs rclone's exit hooks (cleanup of interrupted transfers).
// Call it once the daemon has stopped using the transport.
func Shutdown() { atexit.Run() }

// RcloneVersion reports the version of the embedded rclone engine.
func RcloneVersion() string {
	return fs.Version
}
