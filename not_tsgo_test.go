// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build !tailscale_go

// This file is built when the tailscale_go build tag is NOT set.
// It exists to make the test binary fail loudly when somebody runs
// the test suite without selecting the Tailscale Go toolchain.
//
// The real per-deviation tests live in tsgo_test.go, which only
// builds under //go:build tailscale_go.
//
// See https://github.com/tailscale/go for the fork itself and
// https://github.com/tailscale/go/issues for the list of open
// deviations we maintain. Each test in tsgo_test.go corresponds to
// one such issue.

package tsgotest

import "testing"

// TestTailscaleGoBuildTagRequired fails unconditionally when the
// tailscale_go build tag is not set. It enforces that the rebase
// verification suite is always run against the Tailscale Go
// toolchain. Re-run with:
//
//	go test -tags tailscale_go ./...
//
// using the toolchain at ~/src/tailscale.com/tool/go (see
// tailscale.com/tool/gocross).
func TestTailscaleGoBuildTagRequired(t *testing.T) {
	t.Fatal("this test suite must be built with -tags tailscale_go using the Tailscale Go toolchain; see https://github.com/tailscale/go")
}
