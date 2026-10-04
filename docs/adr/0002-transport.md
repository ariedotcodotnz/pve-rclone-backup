# ADR 0002 — Embedded rclone transport (M1 spike results)

- Status: accepted (go)
- Date: 2026-10-04
- rclone: v1.75.1 (Go module `github.com/rclone/rclone`), Go ≥ 1.26

## Context

ADR 0001 chose to embed rclone as a Go library rather than drive `rclone`
subprocesses or an `rclone rcd` RC server. Milestone M1 had to prove three
things before we commit to that:

1. Byte ranges of a local archive can be uploaded as encrypted objects with a known size, with no temporary copy.
2. Integrity can be verified both during and after upload without downloading.
3. OneDrive's OAuth flow can be driven headlessly in-process.

## What was built

`internal/transport`:

- **`Init`** configures rclone's process-wide state once. It takes an optional custom `config.Storage`, applies low-level retries, and disables multi-thread streams.
- **`Target`** wraps an rclone `fs.Fs`, optionally a crypt remote with the base Fs underneath. It offers `PutSegment`, `Stat`, `VerifySegment`, `Open`, `Remove` and `About`.
- **`segmentObject`** is a read-only `fs.Object` over an `io.ReaderAt` section.
  - It honours `RangeOption` and `SeekOption`.
  - It reports `NoMultiThreading`, so each segment is read as one sequential stream.
  - It hashes only full reads. **Every full `Open` starts a fresh hashing attempt**, so low-level retries never hash bytes twice.
- **Resumable whole-archive SHA-256.** `crypto/sha256` state is marshalled after each segment and restored to continue.
- **OAuth relay helpers.** `ProviderAuthURL` and `RelayRedirect`.
- **`MemoryStorage`**, an in-memory `config.Storage` for tests and fixtures.
- **`test/manual/onedrive-smoke`**, an interactive end-to-end check against a real OneDrive account.

## rclone APIs used

- `fs.NewFs`, `fs.UnWrapObject`, `fs.Usage` via `Features().About`
- `operations.Copy(ctx, f, nil, remote, src fs.Object)`, which brings low-level retries, accounting and size verification
- `(*crypt.Fs).UnWrap`, `(*crypt.Fs).ComputeHash` (in-process cryptcheck), `*crypt.Object`
- `config.SetConfigPath`, `config.SetData`, `config.CreateRemote`/`config.UpdateRemote` with `UpdateRemoteOpt{NonInteractive|Continue}`
- `rc.Calls.Get("config/oauthstatus")`, called in-process with no RC server
- `fs/hash.MultiHasher` for provider hashes of plaintext on unencrypted targets

## Findings

| # | Finding | Consequence |
|---|---|---|
| 1 | Uploading through `operations.Copy` with a known-size source never spooled to `TMPDIR`. Tested by pointing `TMPDIR` at an empty directory, for all three crypt name encodings. | The "no duplicate local copy" requirement holds for any archive size |
| 2 | crypt's `put` hashes the ciphertext with the base remote's hash (MD5 on `local`, QuickXorHash on OneDrive) and compares it with the provider's value, deleting the object on mismatch | Transport verification during upload costs no extra I/O. For plain targets, `PutSegment` computes the provider's hash type over the plaintext itself and compares it the same way. |
| 3 | `VerifySegment` uses `crypt.ComputeHash`, which reads the 32-byte header for the nonce and re-encrypts the local bytes, and correctly detects a one-byte difference | Resuming after a lost database can reuse uploaded segments without downloading them |
| 4 | A transient read fault was retried transparently by rclone's low-level retries, which re-opened the source; the hashes stayed correct. A persistent fault fails `PutSegment` and leaves no object behind. | Our job layer resumes at segment granularity; per-attempt hashing is required, and is covered by tests |
| 5 | `config.SetData` is a **no-op unless `config.SetConfigPath` was given a non-empty path** | `Init` requires `ConfigPath` whenever a custom `Storage` is set |
| 6 | If `Storage.Load` returns any error other than `config.ErrorConfigFileNotFound`, rclone calls `fs.Fatalf` and **exits the process** | The pmxcfs-backed storage (M7) must never return other load errors; it logs them and serves the last good state |
| 7 | `*crypt.Fs` does not export its cipher's `EncryptedSize` | Added `CryptStoredSize`, tested against `crypt.NewCipher(...).EncryptedSize` |
| 8 | `config/oauthstatus` only exposes a loopback `http://127.0.0.1:53682/auth?state=…` URL. That URL 307-redirects to the provider. | `ProviderAuthURL` follows that redirect locally and returns the real provider URL. The user then pastes the failed `http://localhost:53682/?code=…&state=…` address, and `RelayRedirect` replays it to the loopback listener. This works with no SSH tunnel and no rclone on the user's machine. It is tested offline against a fake OAuth provider. |
| 9 | Keys prefixed `config_` are ephemeral. Passing `config_auth_no_browser=true` on the Continue call stops rclone from trying `xdg-open` and is not persisted. | It is used in the relay flow |
| 10 | Importing the library sets `fs.Version` to `v1.75.1-DEV` | The Makefile sets `-X github.com/rclone/rclone/fs.VersionSuffix=` so the reported version is the actual release |
| 11 | Binary size with `local`, `crypt` and `onedrive` compiled in is **~17 MB** (stripped, `-trimpath`, `CGO_ENABLED=0`) | Much smaller than the 40–60 MB estimate; adding backends later is affordable |
| 12 | After `go mod tidy`, the module graph is about 53 indirect requirements | Manageable for vendoring in Debian builds |

## Pending

- **Real OneDrive run.** `go run ./test/manual/onedrive-smoke` needs an interactive Microsoft login. It covers QuickXorHash verification of crypt objects, quota and trashed bytes, and observed throughput.

## Decision

**Go.** Embedding rclone v1.75.x as a library is confirmed as the transport. Fallback, which was not needed: a bundled `rclone rcd` plus chunker, accepting the loss of cross-run resume.

## Follow-ups (M7/M8)

- **pmxcfs-backed `config.Storage`:** atomic writes under a cfs lock, and Load semantics per finding 6.
- **User agent:** set it to identify the project, e.g. `pve-rclone-backup/<ver> rclone/<ver>`. Microsoft recommends decorating traffic to reduce throttling.
- **Logging:** bridge rclone's slog logger into ours, with secret redaction.
- **Error classification:** classify rclone errors (`fserrors`, HTTP status, oauth2 `invalid_grant`) into job error classes.
- **Fault injection:** add a fault-injecting `fs.Fs` decorator for the failure matrix (wrong hash, truncation, 429, quota, auth errors).
- **Accounting:** use per-job stats groups (`accounting.WithStatsGroup`) for progress reporting.
- **Version pinning:** a Renovate/CI policy to track rclone releases.
