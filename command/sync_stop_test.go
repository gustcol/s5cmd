package command

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/request"

	"github.com/peak/s5cmd/v2/storage"
)

func TestShouldStopSync(t *testing.T) {
	t.Parallel()

	testcases := []struct {
		name        string
		exitOnError bool
		err         error
		want        bool
	}{
		{"generic error without exit-on-error", false, errors.New("boom"), false},
		{"generic error with exit-on-error", true, errors.New("boom"), true},
		{"no object found", true, storage.ErrNoObjectFound, false},
		{"access denied", false, awserr.New("AccessDenied", "denied", nil), true},
		{"no such bucket", false, awserr.New("NoSuchBucket", "missing", nil), true},
		{"context canceled with exit-on-error", true, context.Canceled, false},
		{"wrapped context canceled with exit-on-error", true, fmt.Errorf("list: %w", context.Canceled), false},
		{"aws request canceled with exit-on-error", true, awserr.New(request.CanceledErrorCode, "request context canceled", context.Canceled), false},
	}

	for _, tc := range testcases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := Sync{exitOnError: tc.exitOnError}
			if got := s.shouldStopSync(tc.err); got != tc.want {
				t.Errorf("shouldStopSync(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
