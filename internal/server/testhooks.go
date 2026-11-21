//go:build testhooks

package server

// Test hooks, compiled only with "go build -tags testhooks" (the external
// test suite builds cbshare that way). Release builds never contain them.

import (
	"os"
	"time"
)

func init() {
	// CBSHARE_TEST_ENROLMENT_WINDOW shortens the time a new QR code waits for
	// its first sign-in (24 hours), so that tests can see it expire. It can
	// only shorten it.
	if v := os.Getenv("CBSHARE_TEST_ENROLMENT_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 && d < firstSignInWindow {
			firstSignInWindow = d
		}
	}
}
