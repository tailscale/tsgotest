// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build tailscale_go

// Package tsgotest exercises every deviation our Tailscale Go fork
// ("tsgo", https://github.com/tailscale/go) carries on top of
// upstream Go. We rebase tsgo onto each new Go release roughly every
// six months; this suite is the smoke test we run against the freshly
// rebased toolchain to confirm that no patch silently dropped on the
// floor.
//
// There is one test per open issue at
// https://github.com/tailscale/go/issues. The convention is one open
// issue per deviation; when an issue is closed (either upstreamed or
// reverted), delete the corresponding test here. When a new patch is
// added to tsgo, file a tracking issue and add a test below pointing
// at it.
//
// Tests that can only be observed on a specific GOOS or GOARCH still
// live here so that the issue is not forgotten; they call t.Skip when
// run on the wrong platform.
package tsgotest

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Enforcer counters for issue #55. The enforcers are program-global
// singletons (SetXxxEnforcer panics if called twice), so we install
// them exactly once at package init and let every test in the suite
// see them. The functions are permissive (always return nil) so
// they don't break the other tests' dials/HTTP calls; we only care
// that they're invoked.
var (
	issue55DialCalls          atomic.Int64
	issue55ResolveCalls       atomic.Int64
	issue55RoundTripCalls     atomic.Int64
	issue55ResponseWriteCalls atomic.Int64
)

func init() {
	net.SetDialEnforcer(func(ctx context.Context, addrs []net.Addr) error {
		issue55DialCalls.Add(1)
		return nil
	})
	net.SetResolveEnforcer(func(ctx context.Context, op, network, addr string, hint net.Addr) error {
		issue55ResolveCalls.Add(1)
		return nil
	})
	http.SetRoundTripEnforcer(func(req *http.Request) error {
		issue55RoundTripCalls.Add(1)
		return nil
	})
	http.SetResponseWriteEnforcer(func(req *http.Request) {
		issue55ResponseWriteCalls.Add(1)
	})
}

// TestIssue164_AlwaysSetBuildTag verifies that cmd/go in the
// Tailscale Go toolchain sets the tailscale_go build tag
// automatically, without the caller having to pass -tags.
//
// See https://github.com/tailscale/go/issues/164.
//
// The fact that this file compiles and runs at all is a partial
// assertion: every file in this package is gated on
// //go:build tailscale_go, so the test binary would be empty without
// the tag coming from somewhere. To distinguish "tag was passed by
// the caller" from "tag was injected by cmd/go", we run `go list` in
// a hermetic probe package whose only file is gated on
// //go:build tailscale_go, without passing -tags. The patched cmd/go
// must still report it as buildable.
func TestIssue164_AlwaysSetBuildTag(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/go.mod", []byte("module probe\ngo 1.21\n"), 0644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	probe := "//go:build tailscale_go\n\npackage probe\n\nvar Sentinel = 1\n"
	if err := os.WriteFile(dir+"/probe.go", []byte(probe), 0644); err != nil {
		t.Fatalf("write probe.go: %v", err)
	}
	cmd := exec.Command("go", "list", "-f", "{{.GoFiles}}", ".")
	cmd.Dir = dir
	// Deliberately do NOT pass -tags. The patched cmd/go must add
	// tailscale_go to BuildContext.ToolTags on its own.
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "probe.go") {
		t.Fatalf("go list did not pick up probe.go; tailscale_go ToolTag not auto-set by cmd/go.\nOutput: %s", out)
	}
}

// TestIssue166_ToolchainRevInBuildInfo verifies that the Tailscale
// Go toolchain exposes its git revision two ways: as the exported
// runtime/debug.TailscaleToolchainGitRev() function, and as the
// tailscale.toolchain.rev key in every binary's BuildInfo
// settings. tailscale.com surfaces the rev in `tailscale version`
// output, and cmd/go uses the same value to seed its build cache
// keys so re-cherry-picks invalidate stale cached output. Both
// surfaces must agree.
//
// See https://github.com/tailscale/go/issues/166.
func TestIssue166_ToolchainRevInBuildInfo(t *testing.T) {
	rev, ok := debug.TailscaleToolchainGitRev()
	if !ok {
		t.Fatal("debug.TailscaleToolchainGitRev returned ok=false; CI/Nix did not substitute the rev placeholder")
	}
	if rev == "" {
		t.Fatal("debug.TailscaleToolchainGitRev returned ok=true with empty rev")
	}
	t.Logf("debug.TailscaleToolchainGitRev = %s", rev)

	bi, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("debug.ReadBuildInfo returned ok=false; cannot inspect build settings")
	}
	var biRev string
	for _, s := range bi.Settings {
		if s.Key == "tailscale.toolchain.rev" {
			biRev = s.Value
			break
		}
	}
	if biRev == "" {
		var keys []string
		for _, s := range bi.Settings {
			keys = append(keys, s.Key)
		}
		t.Fatalf("tailscale.toolchain.rev not found in debug.ReadBuildInfo settings; got keys: %v", keys)
	}
	if biRev != rev {
		t.Fatalf("rev mismatch: TailscaleToolchainGitRev = %q, BuildInfo tailscale.toolchain.rev = %q", rev, biRev)
	}
}

// TestIssue109_RuntimeTailscaleCurrentP verifies that
// runtime.TailscaleCurrentP returns the current P id. tailscale.com's
// syncs.ShardValue uses it as a cheap, best-effort per-CPU shard key
// (see tailscale.com/syncs/shardvalue_tailscale.go).
//
// See https://github.com/tailscale/go/issues/109.
func TestIssue109_RuntimeTailscaleCurrentP(t *testing.T) {
	// Pin to a single OS thread so the P id can't change mid-call.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	p := runtime.TailscaleCurrentP()
	if p < 0 {
		t.Fatalf("runtime.TailscaleCurrentP() = %d; want >= 0", p)
	}
	if p >= runtime.GOMAXPROCS(0) {
		// Not strictly an error (Ps can outnumber GOMAXPROCS in
		// transient states), but worth flagging.
		t.Logf("runtime.TailscaleCurrentP() = %d, GOMAXPROCS = %d", p, runtime.GOMAXPROCS(0))
	}
}

// TestIssue141_RuntimeTailscaleNumTimers verifies that
// runtime.TailscaleNumTimers reports the total and zombie timer
// counts the runtime is currently tracking. The Tailscale daemon
// uses these to diagnose timer leaks in long-running processes.
//
// See https://github.com/tailscale/go/issues/141.
func TestIssue141_RuntimeTailscaleNumTimers(t *testing.T) {
	total, zombies := runtime.TailscaleNumTimers()
	if total < 0 || zombies < 0 {
		t.Fatalf("runtime.TailscaleNumTimers() = (%d, %d); want non-negative", total, zombies)
	}
	if zombies > total {
		t.Fatalf("runtime.TailscaleNumTimers() = (%d, %d); zombies must not exceed total", total, zombies)
	}
	t.Logf("runtime.TailscaleNumTimers() = (total=%d, zombies=%d)", total, zombies)
}

// TestIssue58_NetSockTrace verifies that the net package exposes the
// SockTrace hook type and the WithSockTrace/ContextSockTrace
// helpers, and that DidRead/DidWrite actually fire for sockets
// dialed with a tracing context. tailscale.com/net/sockstats relies
// on these to attribute every read and write to a logical Tailscale
// connection label, which in turn drives the cellular radio
// battery-use accounting on mobile.
//
// See https://github.com/tailscale/go/issues/58.
//
// Implementation note: in the current tsgo tree the hooks fire only
// from the recv/sendmsg code paths in net/fd_posix.go (readMsg*,
// writeMsg*). Stream Read/Write and the plain readFrom/writeTo
// variants do not invoke them. We deliberately exercise
// ReadMsgUDPAddrPort / WriteMsgUDPAddrPort so a regression that
// drops the hook wiring on the only paths that have it shows up
// here. If a future rebase grows hook coverage to include TCP, add
// a sibling TCP subtest rather than replacing this one.
func TestIssue58_NetSockTrace(t *testing.T) {
	// First: round-trip the trace through a context. This catches
	// the cheaper regression where WithSockTrace / ContextSockTrace
	// disappear from the API entirely.
	traceForCtx := &net.SockTrace{}
	ctxOnly := net.WithSockTrace(context.Background(), traceForCtx)
	if got := net.ContextSockTrace(ctxOnly); got != traceForCtx {
		t.Fatalf("net.ContextSockTrace returned %p; want %p", got, traceForCtx)
	}

	var didRead, didWrite atomic.Int64
	trace := &net.SockTrace{
		DidRead:  func(n int) { didRead.Add(int64(n)) },
		DidWrite: func(n int) { didWrite.Add(int64(n)) },
	}
	ctx := net.WithSockTrace(context.Background(), trace)

	// Both ends are dialed with the tracing context so each side's
	// fd carries hooks, and we use the Msg variants so the hooks
	// actually run.
	serverPC, err := (&net.ListenConfig{}).ListenPacket(ctx, "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}
	defer serverPC.Close()
	server := serverPC.(*net.UDPConn)

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		server.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 32)
		n, _, _, from, err := server.ReadMsgUDPAddrPort(buf, nil)
		if err != nil {
			return
		}
		server.WriteMsgUDPAddrPort(buf[:n], nil, from)
	}()

	var d net.Dialer
	c, err := d.DialContext(ctx, "udp", server.LocalAddr().String())
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	defer c.Close()
	uc := c.(*net.UDPConn)

	// uc is already connected to server.LocalAddr() via DialContext;
	// pass a zero AddrPort to use the connected peer.
	if _, _, err := uc.WriteMsgUDPAddrPort([]byte("ping"), nil, netip.AddrPort{}); err != nil {
		t.Fatalf("WriteMsgUDPAddrPort: %v", err)
	}
	uc.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 32)
	if _, _, _, _, err := uc.ReadMsgUDPAddrPort(buf, nil); err != nil {
		t.Fatalf("ReadMsgUDPAddrPort: %v", err)
	}
	<-serverDone

	if got := didWrite.Load(); got == 0 {
		t.Errorf("DidWrite hook was never invoked; want >0 bytes")
	} else {
		t.Logf("DidWrite total = %d", got)
	}
	if got := didRead.Load(); got == 0 {
		t.Errorf("DidRead hook was never invoked; want >0 bytes")
	} else {
		t.Logf("DidRead total = %d", got)
	}
}

// TestIssue58_NetSockTraceTCP verifies the DidCreateTCPConn and
// WillCloseTCPConn SockTrace hooks, which fire when a TCP socket is
// created or closed with a tracing context. tailscale.com uses them
// to read TCP stats (e.g. retransmit counters) from the raw conn.
// These hooks were added in a follow-up to the original SockTrace
// patch and had a merge conflict in the Go 1.27 rebase (upstream's
// newFD signature changed in net/sock_posix.go), so they get their
// own test.
//
// See https://github.com/tailscale/go/issues/58.
func TestIssue58_NetSockTraceTCP(t *testing.T) {
	var created, closed atomic.Int64
	trace := &net.SockTrace{
		DidCreateTCPConn: func(c syscall.RawConn) {
			if c == nil {
				t.Error("DidCreateTCPConn called with nil RawConn")
			}
			created.Add(1)
		},
		WillCloseTCPConn: func(c syscall.RawConn) {
			if c == nil {
				t.Error("WillCloseTCPConn called with nil RawConn")
			}
			closed.Add(1)
		},
	}
	ctx := net.WithSockTrace(context.Background(), trace)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	if got := created.Load(); got == 0 {
		t.Error("DidCreateTCPConn hook was never invoked")
	}
	if got := closed.Load(); got != 0 {
		t.Errorf("WillCloseTCPConn hook invoked %d times before Close", got)
	}
	c.Close()
	if got := closed.Load(); got == 0 {
		t.Error("WillCloseTCPConn hook was never invoked after Close")
	}
}

// TestIssue55_NetEnforcers verifies the four program-global
// enforcement hooks the Tailscale Go fork adds for control-plane
// safety nets: net.SetDialEnforcer, net.SetResolveEnforcer,
// net/http.SetRoundTripEnforcer, and
// net/http.SetResponseWriteEnforcer. The control plane uses these
// to refuse network operations attempted while holding a lock or
// inside a database transaction.
//
// See https://github.com/tailscale/go/issues/55.
//
// The enforcers are installed at package init (above) with
// permissive bodies that just count invocations, so they don't
// affect the other tests. Here we trigger each code path and
// assert the counters moved. The signatures of the Set*Enforcer
// functions are pinned at the init-time call sites, so a signature
// change during a rebase fails the build.
func TestIssue55_NetEnforcers(t *testing.T) {
	// --- net.SetDialEnforcer + net.SetResolveEnforcer ---
	// resolveAddrList runs the resolve enforcer for any tcp/udp
	// network, even when the address is a literal IP, and
	// dialSerial runs the dial enforcer once it has the addr list.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	dialBefore := issue55DialCalls.Load()
	resolveBefore := issue55ResolveCalls.Load()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	c.Close()
	if got := issue55DialCalls.Load(); got <= dialBefore {
		t.Errorf("dialEnforcer was not invoked by net.Dial; before=%d after=%d", dialBefore, got)
	}
	if got := issue55ResolveCalls.Load(); got <= resolveBefore {
		t.Errorf("resolveEnforcer was not invoked by net.Dial; before=%d after=%d", resolveBefore, got)
	}

	// --- net/http.SetRoundTripEnforcer + SetResponseWriteEnforcer ---
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("pong"))
	}))
	defer srv.Close()

	rtBefore := issue55RoundTripCalls.Load()
	rwBefore := issue55ResponseWriteCalls.Load()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	resp.Body.Close()
	if got := issue55RoundTripCalls.Load(); got <= rtBefore {
		t.Errorf("roundTripEnforcer was not invoked by http.Get; before=%d after=%d", rtBefore, got)
	}
	if got := issue55ResponseWriteCalls.Load(); got <= rwBefore {
		t.Errorf("responseWriteEnforcer was not invoked by the test server; before=%d after=%d", rwBefore, got)
	}
}

// TestIssue55_ResponseWriteEnforcerHTTP2 verifies that the response
// write enforcer also fires on the HTTP/2 server path. This became a
// distinct code path in the Go 1.27 rebase: upstream deleted
// h2_bundle.go in favor of the net/http/internal/http2 package, so
// the fork's hook moved from the bundle into that package
// (http2.ResponseWriteEnforcer). A regression that wires up only the
// HTTP/1 path would pass TestIssue55_NetEnforcers and be caught here.
//
// See https://github.com/tailscale/go/issues/55.
func TestIssue55_ResponseWriteEnforcerHTTP2(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("pong"))
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	rwBefore := issue55ResponseWriteCalls.Load()
	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("response was %s; want HTTP/2.0 (test server misconfigured)", resp.Proto)
	}
	if got := issue55ResponseWriteCalls.Load(); got <= rwBefore {
		t.Errorf("responseWriteEnforcer was not invoked on the HTTP/2 path; before=%d after=%d", rwBefore, got)
	}
}

// TestIssue115_ForbidReflectLinkerFlag verifies the cmd/link
// -forbidreflect flag (and the `-X opt.forbidreflect=1` alias).
// When the linker sees a SymFlagReflectMethod symbol (which the
// compiler emits for reflect.Type.Method-style lookups), the
// deadcode pass conservatively keeps all exported methods of all
// reachable types, which bloats Tailscale binaries. The patch lets
// us fail the link instead of silently shipping a bloated binary.
//
// See https://github.com/tailscale/go/issues/115.
//
// We exercise both arms:
//   - a program with no reflect must link cleanly under -forbidreflect
//   - a program that calls reflect.Type.Method must fail to link
//     with "unexpected reflect with -forbidreflect" in the output
//
// We additionally check the `-X opt.forbidreflect=1` alias triggers
// the same path, since Tailscale's release infra uses it as the
// portable spelling that also works on unmodified upstream Go.
func TestIssue115_ForbidReflectLinkerFlag(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/go.mod", []byte("module probe\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cleanSrc := `package main

func main() { println("hello") }
`
	reflectSrc := `package main

import "reflect"

type T struct{}

func (T) Hello() string { return "hi" }

func main() {
	rt := reflect.TypeOf(T{})
	for i := 0; i < rt.NumMethod(); i++ {
		println(rt.Method(i).Name)
	}
}
`

	build := func(src, ldflags string) (stdoutErr string, ok bool) {
		if err := os.WriteFile(dir+"/main.go", []byte(src), 0644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("go", "build", "-ldflags="+ldflags, "-o", dir+"/probe.bin", ".")
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		return string(out), err == nil
	}

	for _, ldflags := range []string{"-forbidreflect", "-X opt.forbidreflect=1"} {
		// Clean build: must succeed.
		if out, ok := build(cleanSrc, ldflags); !ok {
			t.Fatalf("clean build with ldflags=%q failed:\n%s", ldflags, out)
		}
		// Reflect build: must fail with the expected panic.
		out, ok := build(reflectSrc, ldflags)
		if ok {
			t.Fatalf("reflect build with ldflags=%q unexpectedly succeeded; tsgo forbidreflect patch missing.", ldflags)
		}
		if !strings.Contains(out, "unexpected reflect with -forbidreflect") {
			t.Fatalf("reflect build with ldflags=%q failed but not with the expected panic message; got:\n%s", ldflags, out)
		}
	}
}

// TestIssue149_GoTestCachelinkFlag verifies that `go test` accepts the
// -cachelink flag. The Tailscale fork adds it so distributed test
// shards backed by a shared GOCACHEPROG don't re-link the test
// binary for every -test.run invocation.
//
// See https://github.com/tailscale/go/issues/149.
//
// The flag is registered with an empty usage string, so it does not
// appear in `go help testflag`. We instead probe acceptance by
// invoking `go test -cachelink` against a hermetic noop test
// package in a temp dir and asserting that flag parsing succeeds.
// A bogus flag is run as a control to confirm the probe harness
// would actually catch a missing patch.
func TestIssue149_GoTestCachelinkFlag(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/go.mod", []byte("module probe\ngo 1.21\n"), 0644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(dir+"/x_test.go", []byte("package probe\nimport \"testing\"\nfunc TestNoop(t *testing.T) {}\n"), 0644); err != nil {
		t.Fatalf("write x_test.go: %v", err)
	}

	run := func(flag string) (string, error) {
		cmd := exec.Command("go", "test", flag, "-run", "NONE_PROBE_XXX", ".")
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// Control: an obviously bogus flag must produce a flag-parse error.
	if out, err := run("-cachelink-not-a-real-flag"); err == nil || !strings.Contains(out, "flag provided but not defined") {
		t.Fatalf("control probe failed: expected unknown-flag error for bogus flag, got err=%v out=%s", err, out)
	}

	// Real flag: must be accepted.
	out, err := run("-cachelink")
	if err != nil {
		t.Fatalf("go test -cachelink: %v\n%s", err, out)
	}
	if strings.Contains(out, "flag provided but not defined") {
		t.Fatalf("go test rejected -cachelink; tsgo patch missing.\nOutput:\n%s", out)
	}
}

// TestIssue150_GitHashCacheKeys verifies the opt-in patch that
// makes cmd/go derive cache keys from git blob hashes rather than
// file modtimes. The feature is gated on CMD_GO_USE_GIT_HASH=true
// (see tailscale/corp#37473 for the production rollout).
//
// See https://github.com/tailscale/go/issues/150.
//
// The patched code paths are cmd/go/internal/test/test.go's
// hashOpen/hashWriteStat (test input files) and modindex/read.go's
// indexing fingerprint. Both swap info.ModTime() for the git blob
// hash when CMD_GO_USE_GIT_HASH=true. We exercise the test-input
// path: in a hermetic git repo, a test reads a testdata file. We
// prime the test cache, change only the testdata file's modtime
// (not its content), and confirm the second run is still cached.
// A negative control with CMD_GO_USE_GIT_HASH=false confirms the
// modtime change *would* bust the cache without the patch.
func TestIssue150_GitHashCacheKeys(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}

	dir := t.TempDir()
	home := t.TempDir()

	if err := os.WriteFile(dir+"/go.mod", []byte("module probe\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}
	testSrc := `package probe

import (
	"os"
	"testing"
)

func TestReadsTestdata(t *testing.T) {
	if _, err := os.ReadFile("testdata/input.txt"); err != nil {
		t.Fatal(err)
	}
}
`
	if err := os.WriteFile(dir+"/x_test.go", []byte(testSrc), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir+"/testdata", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/testdata/input.txt", []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}

	gitEnv := append(os.Environ(),
		"HOME="+home,
		"GIT_AUTHOR_NAME=tsgotest", "GIT_AUTHOR_EMAIL=tsgotest@example.com",
		"GIT_COMMITTER_NAME=tsgotest", "GIT_COMMITTER_EMAIL=tsgotest@example.com",
	)
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "."},
		{"-c", "commit.gpgsign=false", "commit", "-q", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = gitEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// hashOpen rejects files whose modtime is within the last 2s
	// (modTimeCutoff in cmd/go/internal/test/test.go), so push
	// every input back in time before any go test runs see them.
	past := time.Now().Add(-time.Hour)
	for _, p := range []string{"go.mod", "x_test.go", "testdata/input.txt"} {
		if err := os.Chtimes(dir+"/"+p, past, past); err != nil {
			t.Fatal(err)
		}
	}

	run := func(gocache, useGitHash string) string {
		cmd := exec.Command("go", "test", ".")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"HOME="+home,
			"GOCACHE="+gocache,
			"CMD_GO_USE_GIT_HASH="+useGitHash,
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go test (CMD_GO_USE_GIT_HASH=%s): %v\n%s", useGitHash, err, out)
		}
		return string(out)
	}

	touch := func() {
		// Pick a recent-but-not-too-recent mtime so hashOpen's
		// 2-second cutoff doesn't reject the file. Far enough
		// from the prior mtime that filesystems with 1s
		// resolution will still register a change.
		newMtime := time.Now().Add(-10 * time.Second)
		if err := os.Chtimes(dir+"/testdata/input.txt", newMtime, newMtime); err != nil {
			t.Fatal(err)
		}
	}

	// Negative control: without the patch, touching a testdata
	// input must invalidate the test result cache. Run this first
	// so a failure here marks the test inconclusive rather than
	// passing the positive case by coincidence.
	gocacheCtrl := t.TempDir()
	run(gocacheCtrl, "false")
	if out := run(gocacheCtrl, "false"); !strings.Contains(out, "(cached)") {
		t.Fatalf("warm run with CMD_GO_USE_GIT_HASH=false did not hit cache; cannot establish baseline.\n%s", out)
	}
	touch()
	if out := run(gocacheCtrl, "false"); strings.Contains(out, "(cached)") {
		t.Skipf("control: touching testdata did NOT bust the cache with CMD_GO_USE_GIT_HASH=false; can't distinguish patched behavior on this platform.\n%s", out)
	}

	// Reset the testdata mtime so the positive case starts from a
	// matching on-disk state.
	if err := os.Chtimes(dir+"/testdata/input.txt", past, past); err != nil {
		t.Fatal(err)
	}

	// Positive case: with the patch, the cache key uses the git
	// blob hash of testdata/input.txt, so changing its mtime alone
	// must not invalidate the cache.
	gocachePos := t.TempDir()
	run(gocachePos, "true")
	if out := run(gocachePos, "true"); !strings.Contains(out, "(cached)") {
		t.Fatalf("warm run with CMD_GO_USE_GIT_HASH=true did not hit cache:\n%s", out)
	}
	touch()
	if out := run(gocachePos, "true"); !strings.Contains(out, "(cached)") {
		t.Fatalf("after touch, CMD_GO_USE_GIT_HASH=true did not hit cache; tsgo githash patch missing.\n%s", out)
	}
}

// TestIssue99_NoPidfdOnAndroid verifies the patch that disables
// pidfd-based child reaping on Android. The kernel/userspace
// combinations shipped on Tailscale's supported Android versions
// have a buggy pidfd implementation that breaks the Tailscale app
// when subprocesses are spawned. The patch adds an early
// `if runtime.GOOS == "android" { return false }` to
// os.pidfdWorks in src/os/pidfd_linux.go.
//
// See https://github.com/tailscale/go/issues/99.
//
// We don't need to run on Android to observe this: runtime.GOOS is
// a compile-time constant, so we cross-compile a tiny Go program
// with GOOS=android and disassemble os.pidfdWorks. With the patch
// the body collapses to a "return false" (no CALL instructions);
// without it, pidfdWorks must call into checkPidfdOnce. A linux
// build serves as the negative control: there the GOOS guard is
// dead-eliminated and pidfdWorks still calls checkPidfdOnce, so
// the linux disassembly *must* contain a CALL. If both arms agree
// we know we're looking at the patch, not at the compiler
// dead-coding something we didn't expect.
func TestIssue99_NoPidfdOnAndroid(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/go.mod", []byte("module probe\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Reference os.FindProcess to keep os.pidfdWorks reachable.
	mainSrc := "package main\n\nimport \"os\"\n\nfunc main() { p, _ := os.FindProcess(os.Getpid()); _ = p }\n"
	if err := os.WriteFile(dir+"/main.go", []byte(mainSrc), 0644); err != nil {
		t.Fatal(err)
	}

	disasm := func(goos string) string {
		bin := dir + "/probe." + goos
		build := exec.Command("go", "build", "-gcflags=all=-l", "-o", bin, ".")
		build.Dir = dir
		build.Env = append(os.Environ(),
			"GOOS="+goos,
			"GOARCH=arm64", // android/amd64 requires cgo external linking; arm64 is the relevant Android target anyway.
			"CGO_ENABLED=0",
		)
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("go build GOOS=%s: %v\n%s", goos, err, out)
		}
		out, err := exec.Command("go", "tool", "objdump", "-s", `^os\.pidfdWorks$`, bin).CombinedOutput()
		if err != nil {
			t.Fatalf("go tool objdump (%s): %v\n%s", goos, err, out)
		}
		return string(out)
	}

	// Tally CALL/BL instructions in the objdump body. `go tool
	// objdump` emits "CALL" for x86 and "CALL" for arm64 indirect
	// calls; runtime.morestack_noctxt shows up as a tail "CALL ...
	// morestack..." prologue on bodies large enough to need stack
	// growth. We only care about *call sites within the function
	// body*, not the morestack prologue, so we filter that out.
	countCalls := func(s string) int {
		var n int
		for _, line := range strings.Split(s, "\n") {
			if !strings.Contains(line, "CALL") && !strings.Contains(line, " BL ") {
				continue
			}
			if strings.Contains(line, "morestack") {
				continue
			}
			n++
		}
		return n
	}

	androidBody := disasm("android")
	if androidBody == "" {
		t.Fatal("objdump for GOOS=android produced no os.pidfdWorks output; symbol missing")
	}
	if !strings.Contains(androidBody, "RET") {
		t.Fatalf("os.pidfdWorks on android does not end with RET; disassembly:\n%s", androidBody)
	}
	if n := countCalls(androidBody); n != 0 {
		t.Fatalf("os.pidfdWorks on android contains %d call(s); tsgo no-pidfd-on-android patch missing.\n%s", n, androidBody)
	}

	// Negative control: linux build must still contain at least
	// one call (to checkPidfdOnce). If it doesn't, the compiler
	// has dead-coded the whole function for unrelated reasons and
	// our positive case proved nothing.
	linuxBody := disasm("linux")
	if linuxBody == "" {
		t.Skip("objdump for GOOS=linux produced no os.pidfdWorks output; control inconclusive")
	}
	if n := countCalls(linuxBody); n == 0 {
		t.Skipf("control: GOOS=linux os.pidfdWorks has no calls either; compiler likely inlined or folded the body. Cannot distinguish patched behavior.\n%s", linuxBody)
	}
}
