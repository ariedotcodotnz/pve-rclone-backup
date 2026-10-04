// SPDX-License-Identifier: AGPL-3.0-or-later

// Package transport wraps the embedded rclone engine.
//
// Archives are stored as fixed-size segments. Each segment is a byte range of
// a local file uploaded as one remote object with a known size, so rclone
// never needs to spool data to a temporary file (OneDrive has no streaming
// uploads). When the target is a crypt remote, rclone verifies the hash of
// the ciphertext against the provider-reported hash during upload; for plain
// remotes this package computes the provider's hash type itself and compares.
//
// rclone keeps process-wide state (configuration, accounting, logging), so
// Init must be called exactly once per process before any other function.
package transport
