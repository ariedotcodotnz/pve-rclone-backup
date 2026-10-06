// SPDX-License-Identifier: AGPL-3.0-or-later

package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/rclone/rclone/backend/crypt"
	onedriveapi "github.com/rclone/rclone/backend/onedrive/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/lib/pacer"
	"golang.org/x/oauth2"
)

// Class is the handling category of a transport error.
type Class string

// Error classes. The job layer maps each class to a retry policy.
const (
	ClassTransient Class = "transient_network" // retry with backoff
	ClassThrottled Class = "throttled"         // retry after the provider's delay
	ClassAuth      Class = "auth_required"     // pause the remote until it is reconnected
	ClassQuota     Class = "quota_exceeded"    // pause the remote until space is available
	ClassIntegrity Class = "integrity"         // re-upload, a bounded number of times
	ClassNotFound  Class = "not_found"         // object or directory missing
	ClassConfig    Class = "config"            // permanent until the configuration changes
	ClassCanceled  Class = "canceled"          // context cancelled
	ClassLocalIO   Class = "local_io"          // reading or writing local files failed
	ClassUnknown   Class = "unknown"           // retry with backoff, bounded attempts
)

// ErrIntegrity marks stored data that does not match what was uploaded.
var ErrIntegrity = errors.New("transport: integrity check failed")

var (
	quotaCodes = []string{"quotaLimitReached", "insufficientStorage", "QuotaExceeded"}
	authCodes  = []string{"InvalidAuthenticationToken", "unauthenticated", "accessDenied"}
	throttle   = []string{"activityLimitReached", "TooManyRequests", "throttledRequest", "serviceNotAvailable"}
	// rclone's token source flattens a rejected refresh (HTTP 400/401) into
	// text such as "invalid_grant: maybe token expired? - try refreshing with
	// "rclone config reconnect ..."". Its "couldn't fetch token" prefix also
	// wraps network failures and server errors, so it says nothing by itself.
	authTexts = []string{"invalid_grant", "invalid_client", "unauthorized_client", "AADSTS70000", "AADSTS700082",
		"AADSTS50173", "token has expired", "expired_token", "re-authenticate", "config reconnect"}
)

// http429Re matches an HTTP 429 status in an error message, but not a 429
// that is merely part of a path or name (such as guest ID 429).
var http429Re = regexp.MustCompile(`(?i)\b429 too many requests\b|\b(?:http|status|status code|response|error)[: ]+429\b`)

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// RetryAfter returns the delay a provider asked for before retrying, if
// the error carries one (HTTP Retry-After).
func RetryAfter(err error) (time.Duration, bool) {
	return pacer.IsRetryAfter(err)
}

// Classify maps an error from rclone or this package to a Class.
func Classify(err error) Class {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.Canceled):
		return ClassCanceled
	case errors.Is(err, ErrIntegrity), strings.Contains(err.Error(), "corrupted on transfer"),
		// Stored ciphertext that does not decrypt or authenticate.
		errors.Is(err, crypt.ErrorEncryptedBadBlock), errors.Is(err, crypt.ErrorEncryptedBadMagic),
		errors.Is(err, crypt.ErrorEncryptedFileTooShort), errors.Is(err, crypt.ErrorEncryptedFileBadHeader):
		return ClassIntegrity
	case errors.Is(err, fs.ErrorObjectNotFound), errors.Is(err, fs.ErrorDirNotFound):
		return ClassNotFound
	}

	if re, ok := errors.AsType[*oauth2.RetrieveError](err); ok {
		if re.ErrorCode == "invalid_grant" || re.ErrorCode == "invalid_client" || re.ErrorCode == "unauthorized_client" {
			return ClassAuth
		}
		// The token endpoint is unavailable rather than refusing us.
		if re.Response != nil {
			switch code := re.Response.StatusCode; {
			case code == http.StatusTooManyRequests:
				return ClassThrottled
			case code >= 500:
				return ClassTransient
			}
		}
	}
	if ae, ok := errors.AsType[*onedriveapi.Error](err); ok {
		code := ae.ErrorInfo.Code + " " + ae.ErrorInfo.InnerError.Code
		switch {
		case containsAny(code, quotaCodes):
			return ClassQuota
		case containsAny(code, authCodes):
			return ClassAuth
		case containsAny(code, throttle):
			return ClassThrottled
		case strings.Contains(code, "pathIsTooLong"), strings.Contains(code, "invalidRequest"):
			return ClassConfig
		}
	}
	msg := err.Error()
	switch {
	case containsAny(msg, quotaCodes), strings.Contains(msg, "Insufficient Storage"):
		return ClassQuota
	case containsAny(msg, authTexts):
		return ClassAuth
	}
	if _, ok := pacer.IsRetryAfter(err); ok {
		return ClassThrottled
	}
	if containsAny(msg, throttle) || http429Re.MatchString(msg) {
		return ClassThrottled
	}

	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EIO) || errors.Is(err, syscall.EROFS) {
		return ClassLocalIO
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) ||
		fserrors.IsRetryError(err) || fserrors.ShouldRetry(err) {
		return ClassTransient
	}
	if _, ok := errors.AsType[net.Error](err); ok {
		return ClassTransient
	}
	if fserrors.IsFatalError(err) || fserrors.IsNoRetryError(err) {
		return ClassConfig
	}
	return ClassUnknown
}
