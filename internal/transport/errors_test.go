// SPDX-License-Identifier: AGPL-3.0-or-later

package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"testing"
	"time"

	onedriveapi "github.com/rclone/rclone/backend/onedrive/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/lib/pacer"
	"golang.org/x/oauth2"
)

func odErr(code, inner string) error {
	e := &onedriveapi.Error{}
	e.ErrorInfo.Code, e.ErrorInfo.InnerError.Code, e.ErrorInfo.Message = code, inner, "msg"
	return e
}

func TestClassify(t *testing.T) {
	cases := []struct {
		err  error
		want Class
	}{
		{nil, ""},
		{context.Canceled, ClassCanceled},
		{fmt.Errorf("upload: %w", context.DeadlineExceeded), ClassTransient},
		{fmt.Errorf("x: %w", ErrIntegrity), ClassIntegrity},
		{errors.New("corrupted on transfer: quickxor hashes differ"), ClassIntegrity},
		{fmt.Errorf("stat: %w", fs.ErrorObjectNotFound), ClassNotFound},
		{fs.ErrorDirNotFound, ClassNotFound},
		{fserrors.FatalError(odErr("quotaLimitReached", "")), ClassQuota},
		{odErr("insufficientStorage", ""), ClassQuota},
		{odErr("InvalidAuthenticationToken", ""), ClassAuth},
		{odErr("activityLimitReached", ""), ClassThrottled},
		{odErr("invalidRequest", "pathIsTooLong"), ClassConfig},
		{fmt.Errorf("couldn't fetch token: %w", &oauth2.RetrieveError{ErrorCode: "invalid_grant"}), ClassAuth},
		{errors.New("failed to refresh token: AADSTS70000: The provided grant has expired"), ClassAuth},
		{pacer.RetryAfterError(errors.New("too many requests"), time.Second), ClassThrottled},
		{fserrors.RetryErrorf("connection reset"), ClassTransient},
		{io.ErrUnexpectedEOF, ClassTransient},
		{&os.PathError{Op: "read", Path: "/x", Err: syscall.EIO}, ClassLocalIO},
		{&os.PathError{Op: "write", Path: "/x", Err: syscall.ENOSPC}, ClassLocalIO},
		{fserrors.NoRetryError(errors.New("bad config")), ClassConfig},
		{errors.New("something odd"), ClassUnknown},
	}
	for _, c := range cases {
		if got := Classify(c.err); got != c.want {
			t.Errorf("Classify(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}
