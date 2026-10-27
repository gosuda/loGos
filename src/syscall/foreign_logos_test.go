// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux

package syscall_test

// Fork/exec foreign-admission attacks (owner: fork preflight lane).
//
// What this file is: public-API attacks that prove the fork/exec window
// needs explicit parent-side admission under the moving collector. Every
// test spawns a real child through the production forkAndExec path (or
// the standalone Exec path) with large, freshly allocated argv/env and,
// where lawful, credential/group pointers, then compares the child's
// independently returned output and error against the exact expected
// bytes. No new public API, no change to non-test code.
//
// Legal child context (from actual control flow in exec_linux.go and
// runtime proc.go), which the future preflight must respect:
//   - The parent builds argv/env C strings, mapping buffers, and fd
//     tables, then forkAndExecInChild1 calls runtime_BeforeFork (spoils
//     the stack guard, blocks signals) and rawVforkSyscall clone/clone3.
//   - On return with pid != 0 or err != 0 the PARENT returns immediately
//     out of the child-shared frame; the outer forkAndExecInChild calls
//     runtime_AfterFork (restores the guard, unblocks signals) and only
//     then reads the error pipe. Admission Begin is legal only here in
//     the preemptible parent before BeforeFork; End is legal only in the
//     parent after AfterFork. The child must never Begin/End.
//   - On return with pid == 0 the CHILD continues inside the
//     forkAndExecInChild1 frame under a poisoned stack guard (and shared
//     memory under CLONE_VM/VFORK): it may issue only RawSyscall-class
//     calls (close/read/dup3/execve/write/exit), runtime_AfterForkInChild
//     (runtime package, uninstrumented), and plain reads of the cells the
//     parent admitted. Allocation, STW, lock acquisition, or stack growth
//     in the child corrupts the suspended parent.
//   - The exact cells the child reads, which the future preflight must
//     root: argv0p, the argvp backing plus each element's NUL bytes, the
//     envvp backing plus bytes, chroot/dir bytes, attr (Files backing),
//     sys (Credential struct, Groups backing, Uid/GidMappings slices,
//     AmbientCaps, PidFD target, cloneArgs), plus the derived buffers
//     built in forkAndExecInChild1 (fd copy, uidmap/gidmap/setgroups
//     bytes). Raw integers (pipe fds, flags, signals) need no root and
//     must stay bit-identical.
//
// Predicted baseline failures (parent runs the gates; this author runs
// no checks): on the default non-moving build these tests pass. Under
// GOEXPERIMENT=logosgcmoving with the moving cycle enabled and WITHOUT
// the pending fork preflight, each test is expected to fail because the
// C-string backing, slice backings, mapping buffers, and group lists the
// child reads can relocate between conversion and clone, so the child
// observes stale bytes: mismatched echo output, EFAULT/EINVAL from
// execve/setgroups, or wrong uid/gid/groups. After the authorized
// preflight (Begin on the exact caller cells before BeforeFork, End in
// the parent after AfterFork) they must pass on both builds. The pidfd
// and clone3-time tests additionally pin the raw integer/pointer plumbing
// (cloneArgs, pidfd cell, exit signal) that admission must preserve
// bit-identically.

import (
	"flag"
	"fmt"
	"internal/testenv"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
)

const (
	logosForkHelperEnv = "GO_WANT_HELPER_PROCESS"
	logosForkEchoMode  = "logosfork-echo"
	logosForkCredMode  = "logosfork-cred"
	logosForkTimeMode  = "logosfork-time"
	logosForkExec1     = "logosfork-exec1"
	logosForkExec2     = "logosfork-exec2"
	logosForkEnvPrefix = "LOGOSFORK_ENV_"
)

// logosForkLargeArgv builds ~256 distinct ~2KB arguments (~500KB total,
// well under ARG_MAX/MAX_ARG_STRLEN). Fresh heap bytes per element keep
// the relocation window real.
func logosForkLargeArgv() []string {
	const n = 256
	argv := make([]string, n)
	for i := range argv {
		argv[i] = fmt.Sprintf("logosfork-arg-%06d-%s", i, strings.Repeat("a", 1900))
	}
	return argv
}

// logosForkLargeEnv builds ~128 distinct ~1KB environment entries.
func logosForkLargeEnv() []string {
	const n = 128
	env := make([]string, n)
	for i := range env {
		env[i] = fmt.Sprintf("%s%06d=%s", logosForkEnvPrefix, i, strings.Repeat("e", 1000))
	}
	return env
}

// logosForkRenderEcho is the exact byte contract shared by parent and
// helper: argc, indexed argv lines, then sorted env lines. Arguments are
// generated without newlines so line framing is unambiguous.
func logosForkRenderEcho(argv []string, env []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "args %d\n", len(argv))
	for i, a := range argv {
		fmt.Fprintf(&b, "%d %s\n", i, a)
	}
	sorted := slices.Clone(env)
	slices.Sort(sorted)
	fmt.Fprintf(&b, "env %d\n", len(sorted))
	for _, e := range sorted {
		b.WriteString(e)
		b.WriteByte('\n')
	}
	return b.String()
}

// logosForkTestEnv returns the test binary's environment filtered to the
// attack variables, sorted for order-independent comparison.
func logosForkTestEnv() []string {
	var env []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, logosForkEnvPrefix) {
			env = append(env, e)
		}
	}
	slices.Sort(env)
	return env
}

// logosForkChurn keeps the allocator and collector busy while the parent
// spawns, widening the relocation window between C-string conversion and
// clone for the moving collector. It stops when stop closes.
func logosForkChurn(stop <-chan struct{}, sink *atomic.Uint64) {
	for {
		select {
		case <-stop:
			return
		default:
			b := make([]byte, 1<<20)
			var s uint64
			for j := 0; j < len(b); j += 4096 {
				b[j] = byte(j)
				s += uint64(b[j])
			}
			sink.Add(s)
		}
	}
}

// TestLogosForkEchoHelper re-execs via -test.run and echoes its argv and
// attack env exactly, then exits without returning through the test
// framework so stdout stays byte-exact (no PASS banner).
func TestLogosForkEchoHelper(t *testing.T) {
	if os.Getenv(logosForkHelperEnv) != logosForkEchoMode {
		return
	}
	fmt.Print(logosForkRenderEcho(flag.Args(), logosForkTestEnv()))
	os.Exit(0)
}

// TestLogosForkLargeArgvEnv spawns the helper with ~500KB of argv and
// ~128KB of env under allocator churn and demands byte-exact echo.
// Without parent-side fork admission a moving collector can relocate the
// C-string backings between SlicePtrFromStrings conversion and clone,
// and the child then execs or prints stale bytes.
func TestLogosForkLargeArgvEnv(t *testing.T) {
	exe := testenv.Executable(t)
	argv := logosForkLargeArgv()
	addEnv := logosForkLargeEnv()
	want := logosForkRenderEcho(argv, addEnv)

	stop := make(chan struct{})
	var sink atomic.Uint64
	for range 4 {
		go logosForkChurn(stop, &sink)
	}
	defer close(stop)

	for i := range 6 {
		runtime.GC()
		args := append([]string{"-test.run=^TestLogosForkEchoHelper$"}, argv...)
		cmd := testenv.Command(t, exe, args...)
		cmd.Env = append(cmd.Environ(), logosForkHelperEnv+"="+logosForkEchoMode)
		cmd.Env = append(cmd.Env, addEnv...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("iter %d: spawn failed: %v\n%.500s", i, err, out)
		}
		if string(out) != want {
			t.Fatalf("iter %d: child returned %d bytes, want %d exact bytes (argv/env corrupted across fork/exec)", i, len(out), len(want))
		}
	}
	_ = sink.Load()
}

// TestLogosForkCredHelper prints the credential state the fork child
// installed, then exits without a PASS banner.
func TestLogosForkCredHelper(t *testing.T) {
	if os.Getenv(logosForkHelperEnv) != logosForkCredMode {
		return
	}
	groups, err := syscall.Getgroups()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	slices.Sort(groups)
	fmt.Printf("uid=%d\ngid=%d\ngroups=%v\n", syscall.Getuid(), syscall.Getgid(), groups)
	os.Exit(0)
}

// TestLogosForkCredentialGroups exercises the credential and group
// pointer cells the vfork child dereferences for setgroups/setgid/setuid.
// As root it installs 64 supplementary groups (a real heap backing the
// child must read intact); unprivileged it remaps through a user
// namespace, which exercises the uidmap/gidmap byte buffers and the
// credential pointer cells where lawful. Unsupported kernels skip.
func TestLogosForkCredentialGroups(t *testing.T) {
	exe := testenv.Executable(t)
	if os.Getuid() == 0 {
		const n = 64
		groups := make([]uint32, n)
		wantGroups := make([]int, n)
		for i := range groups {
			groups[i] = uint32(i)
			wantGroups[i] = i
		}
		cmd := testenv.Command(t, exe, "-test.run=^TestLogosForkCredHelper$")
		cmd.Env = append(cmd.Environ(), logosForkHelperEnv+"="+logosForkCredMode)
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: 0, Gid: 0, Groups: groups},
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			if testenv.SyscallIsNotSupported(err) {
				t.Skipf("skipping: credential/group change refused by kernel/container policy: %v", err)
			}
			t.Fatalf("spawn failed: %v\n%s", err, out)
		}
		want := fmt.Sprintf("uid=0\ngid=0\ngroups=%v\n", wantGroups)
		if string(out) != want {
			t.Fatalf("got %q want %q (credential/group cells corrupted across fork)", out, want)
		}
		return
	}
	uid, gid := os.Getuid(), os.Getgid()
	cmd := testenv.Command(t, exe, "-test.run=^TestLogosForkCredHelper$")
	cmd.Env = append(cmd.Environ(), logosForkHelperEnv+"="+logosForkCredMode)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER,
		Credential: &syscall.Credential{Uid: 0, Gid: 0},
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: uid, Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: gid, Size: 1},
		},
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		if testenv.SyscallIsNotSupported(err) {
			t.Skipf("skipping: CLONE_NEWUSER unsupported: %v", err)
		}
		t.Fatalf("spawn failed: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(lines) != 3 || lines[0] != "uid=0" || lines[1] != "gid=0" {
		t.Fatalf("got %q want uid=0 gid=0 remap (credential/mapping cells corrupted across fork)", out)
	}
}

// TestLogosForkPidfd spawns through the existing public PidFD path and
// demands both a valid pidfd and byte-exact child output, pinning the
// raw clone plumbing (pidfd cell, exit signal, integer flags) that fork
// admission must preserve bit-identically. Kernels before 5.3 lack
// CLONE_PIDFD and skip.
func TestLogosForkPidfd(t *testing.T) {
	exe := testenv.Executable(t)
	argv := []string{"hello", "pidfd"}
	want := logosForkRenderEcho(argv, nil)

	pidfd := -1
	args := append([]string{"-test.run=^TestLogosForkEchoHelper$"}, argv...)
	cmd := testenv.Command(t, exe, args...)
	cmd.Env = append(cmd.Environ(), logosForkHelperEnv+"="+logosForkEchoMode)
	cmd.SysProcAttr = &syscall.SysProcAttr{PidFD: &pidfd}
	out, err := cmd.CombinedOutput()
	if err != nil {
		if testenv.SyscallIsNotSupported(err) {
			t.Skipf("skipping: CLONE_PIDFD unsupported or denied (needs Linux 5.3+, no seccomp deny): %v", err)
		}
		t.Fatalf("spawn failed: %v\n%s", err, out)
	}
	if pidfd < 0 {
		t.Fatalf("pidfd not written by clone (got %d)", pidfd)
	}
	defer syscall.Close(pidfd)
	if string(out) != want {
		t.Fatalf("child returned %d bytes, want %d exact bytes (output corrupted across pidfd fork/exec)", len(out), len(want))
	}
}

// TestLogosForkTimeHelper prints its time-namespace link plus the echo
// contract, then exits without a PASS banner.
func TestLogosForkTimeHelper(t *testing.T) {
	if os.Getenv(logosForkHelperEnv) != logosForkTimeMode {
		return
	}
	timens, err := os.Readlink("/proc/self/ns/time")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fmt.Print(timens + "\n")
	fmt.Print(logosForkRenderEcho(flag.Args(), logosForkTestEnv()))
	os.Exit(0)
}

// TestLogosForkClone3Time spawns through the existing public clone3 path
// (CLONE_NEWTIME selects the cloneArgs struct) with large argv/env and
// demands both a fresh time namespace and byte-exact echo. This pins the
// clone3 struct cell and proves backings survive the clone3 variant of
// the fork window. Unsupported kernels skip.
func TestLogosForkClone3Time(t *testing.T) {
	self, err := os.Readlink("/proc/self/ns/time")
	if err != nil {
		t.Skipf("skipping: cannot read time namespace: %v", err)
	}
	exe := testenv.Executable(t)
	argv := logosForkLargeArgv()[:64]
	addEnv := logosForkLargeEnv()[:32]
	want := logosForkRenderEcho(argv, addEnv)

	runtime.GC()
	args := append([]string{"-test.run=^TestLogosForkTimeHelper$"}, argv...)
	cmd := testenv.Command(t, exe, args...)
	cmd.Env = append(cmd.Environ(), logosForkHelperEnv+"="+logosForkTimeMode)
	cmd.Env = append(cmd.Env, addEnv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWTIME}
	out, err := cmd.CombinedOutput()
	if err != nil {
		if testenv.SyscallIsNotSupported(err) {
			t.Skipf("skipping: CLONE_NEWTIME unsupported: %v", err)
		}
		t.Fatalf("spawn failed: %v\n%.500s", err, out)
	}
	got := string(out)
	lf := strings.IndexByte(got, '\n')
	if lf < 0 {
		t.Fatalf("malformed helper output %q", got)
	}
	if got[:lf] == self {
		t.Fatalf("time namespace not unshared across clone3 fork")
	}
	if got[lf+1:] != want {
		t.Fatalf("child returned %d bytes after ns line, want %d exact bytes (argv/env corrupted across clone3 fork/exec)", len(got[lf+1:]), len(want))
	}
}

// TestLogosExecLargeEnv proves the standalone Exec path preserves large
// argv/env: the parent spawns stage 1, stage 1 re-execs itself via
// syscall.Exec into stage 2 with 64 large args and 32 large env entries,
// and stage 2 echoes them exactly. The compiler lane covers standalone
// Exec by instrumentation (not by filename exclusion); this test turns
// that coverage into evidence. Without it, a moving collector can
// relocate the env backings between conversion and SYS_EXECVE.
func TestLogosExecLargeEnv(t *testing.T) {
	switch os.Getenv(logosForkHelperEnv) {
	case logosForkExec1:
		args := append([]string{"-test.run=^TestLogosExecLargeEnv$"}, flag.Args()...)
		argv := append([]string{os.Args[0]}, args...)
		if err := os.Setenv(logosForkHelperEnv, logosForkExec2); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if err := syscall.Exec(os.Args[0], argv, os.Environ()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "syscall.Exec returned")
		os.Exit(2)
	case logosForkExec2:
		fmt.Print(logosForkRenderEcho(flag.Args(), logosForkTestEnv()))
		os.Exit(0)
	case "":
		// Parent flow below.
	default:
		return
	}

	exe := testenv.Executable(t)
	argv := logosForkLargeArgv()[:64]
	addEnv := logosForkLargeEnv()[:32]
	want := logosForkRenderEcho(argv, addEnv)

	runtime.GC()
	args := append([]string{"-test.run=^TestLogosExecLargeEnv$"}, argv...)
	cmd := testenv.Command(t, exe, args...)
	cmd.Env = append(cmd.Environ(), addEnv...)
	cmd.Env = append(cmd.Env, logosForkHelperEnv+"="+logosForkExec1)
	// Stage 1 re-execs with the marker flipped to stage 2; os.Setenv in
	// the helper keeps the large env intact across the test-binary spawn.
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("spawn failed: %v\n%.500s", err, out)
	}
	if string(out) != want {
		t.Fatalf("exec chain returned %d bytes, want %d exact bytes (argv/env corrupted across standalone Exec)", len(out), len(want))
	}
}
