package process

import (
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/qemu"
)

func testRunner() execx.Runner { return execx.OSRunner{Timeout: 10 * time.Second} }

func waitForProcessArgv(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if argv, err := processArgv(pid); err == nil && len(argv) > 0 {
			return
		} else {
			lastErr = err
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process argv did not become readable: %v", lastErr)
}

// liveProcess starts a long-lived child and returns its PID and the invocation
// a caller would have recorded for it.
// The argument stands in for the per-node `-uuid` that makes every real Barn
// QEMU command line unique.
func liveProcess(t *testing.T, seconds string) (int, qemu.Invocation) {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep is not installed")
	}
	command := exec.Command(sleep, seconds)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waitForProcessArgv(t, command.Process.Pid)
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
	})
	return command.Process.Pid, qemu.Invocation{Binary: sleep, Args: []string{seconds}}
}

func TestAliveDistinguishesRealProcesses(t *testing.T) {
	t.Parallel()
	if !Alive(os.Getpid()) {
		t.Fatal("the running test process is not reported alive")
	}
	for _, pid := range []int{0, -1, -12345} {
		if Alive(pid) {
			t.Errorf("Alive(%d) = true, want false", pid)
		}
	}
	// A PID above the platform maximum can never name a live process, so no
	// signal can reach it.
	if Alive(1 << 30) {
		t.Error("an impossible PID was reported alive")
	}
}

func TestCaptureRecordsStableIdentityAndRejectsAForeignBinary(t *testing.T) {
	t.Parallel()
	pid, invocation := liveProcess(t, "121")

	identity, err := Capture(context.Background(), testRunner(), invocation, pid)
	if err != nil {
		t.Fatal(err)
	}
	if identity.PID != pid || identity.Executable == "" || identity.Started == "" || len(identity.ArgvHash) != 64 {
		t.Fatalf("identity = %#v", identity)
	}
	if !numericStartPattern.MatchString(identity.Started) {
		t.Fatalf("Capture produced legacy start identity %q", identity.Started)
	}
	if identity.ArgvHash != ExpectedArgvHash(invocation) {
		t.Fatalf("argv hash = %s, want %s", identity.ArgvHash, ExpectedArgvHash(invocation))
	}
	// Capture must be a pure observation: two reads of an unchanged process have
	// to agree, or every later liveness check would report a false mismatch.
	second, err := Capture(context.Background(), testRunner(), invocation, pid)
	if err != nil || second != identity {
		t.Fatalf("Capture is not stable: %#v vs %#v (%v)", second, identity, err)
	}

	// A PID that has been recycled into someone else's program must not be
	// accepted as ours just because the number matches.
	foreign := qemu.Invocation{Binary: "/usr/local/bin/qemu-system-aarch64", Args: invocation.Args}
	if _, err := Capture(context.Background(), testRunner(), foreign, pid); err == nil {
		t.Fatal("Capture accepted a process running a different executable")
	}
}

func TestNumericIdentitySurvivesLocaleAndTimezoneChanges(t *testing.T) {
	t.Setenv("LC_ALL", "C")
	t.Setenv("TZ", "UTC")
	pid, invocation := liveProcess(t, "121")
	identity, err := Capture(context.Background(), testRunner(), invocation, pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LC_ALL", "fr_FR.UTF-8")
	t.Setenv("TZ", "Pacific/Honolulu")
	if !MatchesLive(context.Background(), testRunner(), identity, invocation) {
		t.Fatalf("numeric identity changed across locale/timezone: %#v", identity)
	}
}

func TestMatchesLiveRejectsLegacyStartEncoding(t *testing.T) {
	pid, invocation := liveProcess(t, "121")
	identity, err := Capture(context.Background(), testRunner(), invocation, pid)
	if err != nil {
		t.Fatal(err)
	}
	identity.Started = "Thu Jan  1 00:00:00 1970"
	if MatchesLive(context.Background(), testRunner(), identity, invocation) {
		t.Fatal("accepted retired locale-dependent process identity")
	}
}

func TestExpectedArgvHashPreservesArgumentBoundaries(t *testing.T) {
	invocation := qemu.Invocation{Binary: "/opt/qemu", Args: []string{"-name", "node 1", "-uuid", "abc"}}
	digest := ExpectedArgvHash(invocation)
	if len(digest) != 64 || digest == ExpectedArgvHash(qemu.Invocation{Binary: invocation.Binary, Args: []string{"-name", "other"}}) {
		t.Fatalf("unexpected invocation digest %q", digest)
	}
}

func TestDarwinProcArgsParserPreservesArgumentBoundaries(t *testing.T) {
	data := make([]byte, 4)
	binary.LittleEndian.PutUint32(data, 3)
	data = append(data, []byte("/usr/bin/tool\x00\x00\x00")...)
	data = append(data, []byte("/usr/bin/tool\x00中文 path\x00\x00")...)
	argv, err := parseDarwinProcArgs(data)
	if err != nil || len(argv) != 3 || argv[0] != "/usr/bin/tool" || argv[1] != "中文 path" || argv[2] != "" {
		t.Fatalf("Darwin argv = %#v, %v", argv, err)
	}
}

func TestNumericIdentityBindsNonASCIIArgvInCLocale(t *testing.T) {
	t.Setenv("LC_ALL", "C")
	t.Setenv("TZ", "UTC")
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is not installed")
	}
	invocation := qemu.Invocation{Binary: shell, Args: []string{"-c", "sleep 30; :", "中文参数"}}
	command := exec.Command(invocation.Binary, invocation.Args...)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waitForProcessArgv(t, command.Process.Pid)
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
	})
	identity, err := Capture(context.Background(), testRunner(), invocation, command.Process.Pid)
	if err != nil || identity.ArgvHash != ExpectedArgvHash(invocation) || !MatchesLive(context.Background(), testRunner(), identity, invocation) {
		t.Fatalf("non-ASCII argv identity = %#v, %v", identity, err)
	}
}

func TestCaptureRejectsInvalidArguments(t *testing.T) {
	t.Parallel()
	for name, pid := range map[string]int{"zero": 0, "negative": -1} {
		if _, err := Capture(context.Background(), testRunner(), qemu.Invocation{Binary: "/bin/sleep"}, pid); err == nil {
			t.Errorf("Capture accepted a %s PID", name)
		}
	}
	if _, err := Capture(context.Background(), nil, qemu.Invocation{Binary: "/bin/sleep"}, os.Getpid()); err == nil {
		t.Error("Capture accepted a nil runner")
	}
}

func TestMatchesLiveRequiresEveryRecordedFact(t *testing.T) {
	t.Parallel()
	pid, invocation := liveProcess(t, "121")
	identity, err := Capture(context.Background(), testRunner(), invocation, pid)
	if err != nil {
		t.Fatal(err)
	}
	if !MatchesLive(context.Background(), testRunner(), identity, invocation) {
		t.Fatal("a freshly captured live process did not match itself")
	}

	// Each recorded fact is load-bearing. A state file missing any one of them
	// must not be trusted to identify a process we are about to signal.
	for name, mutate := range map[string]func(Identity) Identity{
		"no executable": func(i Identity) Identity { i.Executable = ""; return i },
		"no start time": func(i Identity) Identity { i.Started = ""; return i },
		"no argv hash":  func(i Identity) Identity { i.ArgvHash = ""; return i },
		"wrong argv hash": func(i Identity) Identity {
			i.ArgvHash = "0000000000000000000000000000000000000000000000000000000000000000"
			return i
		},
		"wrong start time": func(i Identity) Identity { i.Started = "Thu Jan  1 00:00:00 1970"; return i },
		"dead pid":         func(i Identity) Identity { i.PID = 1 << 30; return i },
	} {
		if MatchesLive(context.Background(), testRunner(), mutate(identity), invocation) {
			t.Errorf("MatchesLive accepted an identity with %s", name)
		}
	}
}

func TestMatchesLiveRejectsARecycledPID(t *testing.T) {
	t.Parallel()
	pid, invocation := liveProcess(t, "121")
	identity, err := Capture(context.Background(), testRunner(), invocation, pid)
	if err != nil {
		t.Fatal(err)
	}
	// The number still names a live process, but it is running a different
	// command line. This is the case that would otherwise get an innocent
	// process killed, and the argv hash is what rules it out: every real QEMU
	// command line carries that node's own `-uuid`, so no two Barn guests —
	// and no unrelated program — ever hash the same.
	other, otherInvocation := liveProcess(t, "122")
	identity.PID = other
	if MatchesLive(context.Background(), testRunner(), identity, otherInvocation) {
		t.Fatal("MatchesLive accepted a different live process under a reused PID")
	}
}

func TestObserveProvesPIDReuse(t *testing.T) {
	t.Parallel()
	pid, invocation := liveProcess(t, "123")
	identity, err := Capture(context.Background(), testRunner(), invocation, pid)
	if err != nil {
		t.Fatal(err)
	}
	if got := Observe(context.Background(), testRunner(), identity, invocation); got != Ours {
		t.Fatalf("recorded live process = %v, want Ours", got)
	}
	// A different live process under the recorded PID: its start time differs.
	other, otherInvocation := liveProcess(t, "124")
	reused := identity
	reused.PID = other
	// Linux start identities have clock-tick precision. Both children can be
	// born in one tick; wait until this fixture actually proves a later birth.
	deadline := time.Now().Add(time.Second)
	for {
		started, err := processStarted(reused.PID)
		if err != nil {
			t.Fatal(err)
		}
		if started != identity.Started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("could not create a process with a distinct start identity")
		}
		time.Sleep(20 * time.Millisecond)
		reused.PID, otherInvocation = liveProcess(t, "124")
	}
	if got := Observe(context.Background(), testRunner(), reused, otherInvocation); got != Foreign {
		t.Fatalf("reused PID = %v, want Foreign", got)
	}
	if got := Observe(context.Background(), testRunner(), Identity{PID: 1 << 30}, invocation); got != Dead {
		t.Fatalf("free PID = %v, want Dead", got)
	}
}

func TestObserveProvesARootProcessIsNotQEMU(t *testing.T) {
	t.Parallel()
	// PID 1 answers signal 0 with EPERM, so it is alive but unsignalable; its
	// executable and start time still prove it is not a recorded QEMU.
	recorded := Identity{PID: 1, Executable: "/opt/homebrew/bin/qemu-system-aarch64", Started: "kinfo:1.000000", ArgvHash: "0"}
	if got := Observe(context.Background(), testRunner(), recorded, qemu.Invocation{Binary: recorded.Executable}); got != Foreign {
		t.Fatalf("PID 1 = %v, want Foreign", got)
	}
}
