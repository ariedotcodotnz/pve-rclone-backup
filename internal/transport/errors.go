// SPDX-License-Identifier: AGPL-3.0-or-later

package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"

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
	authTexts  = []string{"invalid_grant", "AADSTS70000", "AADSTS700082", "AADSTS50173", "token has expired",
		"couldn't fetch token", "expired_token", "re-authenticate", "config reconnect"}
)

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// Classify maps an error from rclone or this package to a Class.
func Classify(err error) Class {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.Canceled):
		return ClassCanceled
	case errors.Is(err, ErrIntegrity), strings.Contains(err.Error(), "corrupted on transfer"):
		return ClassIntegrity
	case errors.Is(err, fs.ErrorObjectNotFound), errors.Is(err, fs.ErrorDirNotFound):
		return ClassNotFound
	}

	if re, ok := errors.AsType[*oauth2.RetrieveError](err); ok {
		if re.ErrorCode == "invalid_grant" || re.ErrorCode == "invalid_client" || re.ErrorCode == "unauthorized_client" {
			return ClassAuth
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
	if containsAny(msg, throttle) || strings.Contains(msg, "429") {
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
