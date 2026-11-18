// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

//go:build tailscale_go && linux

package tsgotest

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

// TestIssue195_Accept4FallbackLinuxARM verifies that net listeners on
// linux/arm fall back to accept(2) when the kernel lacks accept4,
// which on ARM only arrived in Linux 2.6.36. Old Synology NASes
// (88f6281/88f6282, DSM 6.2, Linux 2.6.32) need this; upstream Go
// dropped its fallback in Go 1.24.
//
// See https://github.com/tailscale/go/issues/195 and
// https://github.com/tailscale/tailscale/issues/18268.
//
// We can't boot a 2.6.32 ARM kernel here, so we fake one: we
// cross-compile a GOARCH=arm probe and run it under qemu-arm
// (user-mode emulation, which forwards syscalls to the host kernel)
// with a host seccomp filter that makes accept4 fail with ENOSYS
// whenever flags are nonzero. qemu implements the guest's plain
// accept as host accept4(..., 0), so that still works, which is
// exactly what a pre-2.6.36 ARM kernel looks like to Go. The probe
// first checks that a raw syscall.Accept4 really gets ENOSYS (proving
// the filter is in effect), then checks that net.Listener.Accept works
// for TCP and unix sockets and returns nonblocking, close-on-exec fds.
//
// The test needs qemu-arm (Debian/Ubuntu package qemu-user) and a
// linux/amd64 or linux/arm64 host. It skips without them, except when
// $CI is set, where it fails so the test can't silently stop running.
func TestIssue195_Accept4FallbackLinuxARM(t *testing.T) {
	skip := t.Skipf
	if os.Getenv("CI") != "" {
		skip = t.Fatalf
	}
	if _, _, ok := hostAccept4Filter(); !ok {
		skip("unsupported host GOARCH %s for seccomp filter", runtime.GOARCH)
	}
	qemu, err := exec.LookPath("qemu-arm")
	if err != nil {
		if qemu, err = exec.LookPath("qemu-arm-static"); err != nil {
			skip("qemu-arm not found; install qemu-user")
		}
	}

	dir := t.TempDir()
	if err := os.WriteFile(dir+"/go.mod", []byte("module probe\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/main.go", []byte(accept4ProbeSrc), 0644); err != nil {
		t.Fatal(err)
	}
	probe := dir + "/probe.arm"
	build := exec.Command("go", "build", "-o", probe, ".")
	build.Dir = dir
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH=arm", "GOARM=5", "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build GOARCH=arm: %v\n%s", err, out)
	}

	// Re-exec ourselves so the seccomp filter is installed in a
	// throwaway process that then execs qemu-arm.
	cmd := exec.Command(os.Args[0], "-test.run=^TestIssue195_Accept4FallbackLinuxARM$")
	cmd.Env = append(os.Environ(), accept4HelperEnv+"="+qemu, accept4HelperEnv+"_PROBE="+probe)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "PROBE OK") {
		t.Fatalf("probe under qemu-arm with accept4 blocked failed (err=%v); tsgo linux/arm accept fallback missing?\n%s", err, out)
	}
}

const accept4HelperEnv = "TSGOTEST_ACCEPT4_HELPER"

func init() {
	v := os.Getenv(accept4HelperEnv)
	if v == "" {
		return
	}
	qemu, probe := v, os.Getenv(accept4HelperEnv+"_PROBE")
	if err := execWithAccept4Blocked(qemu, []string{qemu, probe}); err != nil {
		println("accept4 helper:", err.Error())
		os.Exit(2)
	}
}

// hostAccept4Filter returns the seccomp audit arch and accept4 syscall
// number for the host, and whether the host is supported.
func hostAccept4Filter() (auditArch, nrAccept4 uint32, ok bool) {
	switch runtime.GOARCH {
	case "amd64":
		return 0xC000003E, 288, true // AUDIT_ARCH_X86_64
	case "arm64":
		return 0xC00000B7, 242, true // AUDIT_ARCH_AARCH64
	}
	return 0, 0, false
}

// execWithAccept4Blocked installs a seccomp filter that makes accept4
// with nonzero flags fail with ENOSYS, then execs argv0 with argv.
func execWithAccept4Blocked(argv0 string, argv []string) error {
	arch, nr, ok := hostAccept4Filter()
	if !ok {
		return syscall.ENOTSUP
	}
	// The filter applies to the calling thread and survives exec,
	// so stay on this thread until we exec.
	runtime.LockOSThread()

	const (
		ldW  = 0x20 // BPF_LD|BPF_W|BPF_ABS
		jeqK = 0x15 // BPF_JMP|BPF_JEQ|BPF_K
		retK = 0x06 // BPF_RET|BPF_K

		seccompRetAllow = 0x7fff0000
		seccompRetErrno = 0x00050000

		offNr    = 0
		offArch  = 4
		offArgs3 = 16 + 3*8 // low 32 bits of flags on little-endian
	)
	type sockFilter struct {
		code   uint16
		jt, jf uint8
		k      uint32
	}
	prog := []sockFilter{
		{ldW, 0, 0, offArch},
		{jeqK, 0, 5, arch}, // wrong arch: allow
		{ldW, 0, 0, offNr},
		{jeqK, 0, 3, nr}, // not accept4: allow
		{ldW, 0, 0, offArgs3},
		{jeqK, 1, 0, 0}, // flags == 0: allow
		{retK, 0, 0, seccompRetErrno | uint32(syscall.ENOSYS)},
		{retK, 0, 0, seccompRetAllow},
	}
	fprog := struct {
		len    uint16
		filter *sockFilter
	}{uint16(len(prog)), &prog[0]}

	const prSetNoNewPrivs = 38
	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); e != 0 {
		return e
	}
	const prSetSeccomp, seccompModeFilter = 22, 2
	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetSeccomp, seccompModeFilter, uintptr(unsafe.Pointer(&fprog)), 0, 0, 0); e != 0 {
		return e
	}
	return syscall.Exec(argv0, argv, os.Environ())
}

// accept4ProbeSrc is the GOARCH=arm program run under qemu-arm with
// accept4 blocked.
const accept4ProbeSrc = `package main

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

func fail(format string, args ...any) {
	fmt.Printf("PROBE FAIL: "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	// Control: the seccomp filter must make accept4 return ENOSYS.
	// Without it, a nonblocking listener with no pending conns
	// returns EAGAIN.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		fail("listen: %v", err)
	}
	rc, _ := ln.(*net.TCPListener).SyscallConn()
	var aerr error
	rc.Control(func(fd uintptr) {
		_, _, aerr = syscall.Accept4(int(fd), syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC)
	})
	if aerr != syscall.ENOSYS {
		fail("control: raw accept4 = %v; want ENOSYS (seccomp filter not active?)", aerr)
	}
	ln.Close()

	for _, na := range [][2]string{
		{"tcp4", "127.0.0.1:0"},
		{"unix", "@tsgotest-accept4-probe"},
	} {
		ln, err := net.Listen(na[0], na[1])
		if err != nil {
			fail("listen %v: %v", na, err)
		}
		c, err := net.Dial(ln.Addr().Network(), ln.Addr().String())
		if err != nil {
			fail("dial %v: %v", na, err)
		}
		sc, err := ln.Accept()
		if err != nil {
			fail("accept %v: %v", na, err)
		}
		if na[0] == "tcp4" && sc.RemoteAddr().String() != c.LocalAddr().String() {
			fail("RemoteAddr = %v; want %v", sc.RemoteAddr(), c.LocalAddr())
		}
		src, _ := sc.(syscall.Conn).SyscallConn()
		var fl, fdfl uintptr
		src.Control(func(fd uintptr) {
			fl, _, _ = syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFL, 0)
			fdfl, _, _ = syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFD, 0)
		})
		if fl&syscall.O_NONBLOCK == 0 || fdfl&syscall.FD_CLOEXEC == 0 {
			fail("%v: accepted fd flags=%#x fdflags=%#x; want O_NONBLOCK and FD_CLOEXEC", na, fl, fdfl)
		}
		c.Write([]byte("hi"))
		buf := make([]byte, 2)
		if _, err := sc.Read(buf); err != nil || string(buf) != "hi" {
			fail("%v: read %q, %v", na, buf, err)
		}
		c.Close()
		sc.Close()
		ln.Close()
	}
	fmt.Println("PROBE OK")
}
`
