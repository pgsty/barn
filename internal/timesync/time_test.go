package timesync

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/vm"
)

type guestFixture struct {
	python        string
	offset        time.Duration
	monotonicBase int64
	synchronized  bool
	wrongIdentity bool
	missingReady  bool
	delay         bool
	hang          bool
	splitNewline  bool
	exitEarly     bool
}

func (f guestFixture) Run(context.Context, string, ...string) (execx.Result, error) {
	panic("clock exchange requires duplex IO")
}

func (f guestFixture) RunWithIO(ctx context.Context, _ string, stdin io.Reader, stdout io.Writer, _ ...string) (execx.Result, error) {
	marker := vm.ReadyMarker{Node: "meta", Generation: 2, SpecHash: "hash"}
	expected, _ := json.Marshal(marker)
	if f.wrongIdentity {
		marker.Node = "other"
	}
	identity, _ := json.Marshal(marker)
	script := guestScript
	if f.delay {
		script = strings.Replace(script, "def reply(value):", "def reply(value):\n    if 'unix_ns' in value: time.sleep(0.3)", 1)
	}
	if f.hang {
		script = "import time; time.sleep(30)"
	}
	if f.exitEarly {
		script = "import sys; sys.exit(255)"
	}
	if f.splitNewline {
		script = strings.Replace(script, "print(json.dumps(value), flush=True)", "sys.stdout.write(json.dumps(value)); sys.stdout.flush(); time.sleep(0.01); print('', flush=True)", 1)
	}
	syncValue := "no"
	leap := "Not synchronised"
	if f.synchronized {
		syncValue = "yes"
		leap = "Normal"
	}
	// Execute the real guest helper, replacing only its privileged clock write,
	// identity file and NTP probes. The test process never sets the host clock.
	wrapper := fmt.Sprintf(`import base64, builtins, io, subprocess, time
offset = %d
mono_base = %d
missing_ready = %s
real_time = time.time
real_monotonic = time.monotonic
start_monotonic = real_monotonic()
time.time = lambda: real_time() + offset / 1_000_000_000
if mono_base > 0:
    time.monotonic = lambda: mono_base / 1_000_000_000 + (real_monotonic() - start_monotonic)
def set_clock(clock, stamp):
    global offset
    offset = int((stamp - real_time()) * 1_000_000_000)
time.clock_settime = set_clock
for name in ('time_ns', 'monotonic_ns', 'clock_settime_ns'):
    if hasattr(time, name): delattr(time, name)
real_open = builtins.open
def open_file(path, *args, **kwargs):
    if path == '/var/lib/barn/ready.json':
        if missing_ready: raise FileNotFoundError(2, 'No such file or directory', path)
        return io.StringIO(base64.b64decode('%s').decode())
    return real_open(path, *args, **kwargs)
builtins.open = open_file
def run(args, **kwargs):
    assert 'capture_output' not in kwargs and 'text' not in kwargs, 'requires Python 3.7'
    assert kwargs.get('stdin') == subprocess.DEVNULL, 'probe subprocess must not inherit request stdin'
    text = 'NTPSynchronized=%s\nNTP=yes\n' if args[0] == 'timedatectl' else 'Reference ID : TEST\nLeap status : %s\n'
    return subprocess.CompletedProcess(args, 0, text, '')
subprocess.run = run
exec(base64.b64decode('%s').decode())
`, int64(f.offset), f.monotonicBase, map[bool]string{true: "True", false: "False"}[f.missingReady], base64.StdEncoding.EncodeToString(identity), syncValue, leap, base64.StdEncoding.EncodeToString([]byte(script)))
	return (execx.OSRunner{WaitDelay: time.Second}).RunWithIO(ctx, f.python, stdin, stdout, "-u", "-c", wrapper, string(expected))
}

func fixture(t *testing.T) guestFixture {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is needed to exercise the Linux guest helper")
	}
	return guestFixture{python: python}
}

func TestGuestClockExchange(t *testing.T) {
	for _, test := range []struct {
		name    string
		offset  time.Duration
		options Options
		action  string
		sync    bool
	}{
		{"status", 7 * time.Second, Options{}, "checked", false},
		{"forward", -30 * time.Second, Options{Sync: true}, "adjusted", false},
		{"backward", 30 * time.Second, Options{Sync: true}, "adjusted", false},
		{"force small offset", 20 * time.Millisecond, Options{Sync: true}, "adjusted", false},
		{"automatic small offset", 500 * time.Millisecond, Options{Sync: true, Automatic: true}, "within-threshold", false},
		{"automatic large offset", -20 * time.Second, Options{Sync: true, Automatic: true}, "adjusted", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			guest := fixture(t)
			guest.offset = test.offset
			guest.synchronized = test.sync
			result, err := Run(context.Background(), guest, "ssh", nil, vm.ReadyMarker{}, test.options)
			if err != nil || result.Action != test.action {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if math.Abs(result.Before.OffsetMS-float64(test.offset)/1e6) > 50 {
				t.Fatalf("wrong measured offset: %+v", result.Before)
			}
			if result.After != nil && math.Abs(result.After.OffsetMS) > 50 {
				t.Fatalf("correction missed host time: %+v", result.After)
			}
			if result.NTP.Synchronized == nil || *result.NTP.Synchronized != test.sync {
				t.Fatalf("lost NTP state: %+v", result.NTP)
			}
		})
	}
}

func TestClockRejectsConflictIdentityAndLatency(t *testing.T) {
	for _, test := range []struct{ name, want string }{
		{"conflict", "drift returned"}, {"identity", "identity does not match"}, {"missing-ready", "guest setup is not complete"}, {"latency", "all round trips exceeded"},
	} {
		t.Run(test.name, func(t *testing.T) {
			guest := fixture(t)
			guest.offset = 10 * time.Second
			guest.synchronized = true
			guest.wrongIdentity = test.name == "identity"
			guest.missingReady = test.name == "missing-ready"
			guest.delay = test.name == "latency"
			options := Options{Sync: true, Automatic: true, RecentlyAdjusted: test.name == "conflict"}
			result, err := Run(context.Background(), guest, "ssh", nil, vm.ReadyMarker{}, options)
			if err == nil || !strings.Contains(err.Error(), test.want) || strings.Contains(err.Error(), "ready.json") || result.Action == "adjusted" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

func TestClockDistinguishesHostSleepAndRebootFromNTPConflict(t *testing.T) {
	now := time.Now().UTC()
	// Host slept 3 minutes during a 5-minute window: wall elapsed is 5m while
	// guest monotonic advanced only 2m. Automatic sync must correct, not conflict.
	sleepGuest := fixture(t)
	sleepGuest.offset = -3 * time.Minute
	sleepGuest.synchronized = true
	sleepGuest.monotonicBase = int64(12 * time.Minute)
	sleepOptions := Options{
		Sync:             true,
		Automatic:        true,
		RecentlyAdjusted: true,
		LastAdjustedAt:   now.Add(-5 * time.Minute),
		LastMonotonicNS:  int64(10 * time.Minute),
	}
	result, err := Run(context.Background(), sleepGuest, "ssh", nil, vm.ReadyMarker{}, sleepOptions)
	if err != nil || result.Action != "adjusted" {
		t.Fatalf("host sleep falsely flagged conflict: result=%+v err=%v", result, err)
	}

	// Continuous execution: both wall and guest monotonic advanced 1m, yet guest
	// drifted 10s while NTP is synchronized -> genuine source conflict.
	continuousGuest := fixture(t)
	continuousGuest.offset = 10 * time.Second
	continuousGuest.synchronized = true
	continuousGuest.monotonicBase = int64(11 * time.Minute)
	continuousOptions := Options{
		Sync:             true,
		Automatic:        true,
		RecentlyAdjusted: true,
		LastAdjustedAt:   now.Add(-time.Minute),
		LastMonotonicNS:  int64(10 * time.Minute),
	}
	result, err = Run(context.Background(), continuousGuest, "ssh", nil, vm.ReadyMarker{}, continuousOptions)
	if err == nil || result.Action != "source-conflict" {
		t.Fatalf("continuous NTP conflict missed: result=%+v err=%v", result, err)
	}

	// Guest reboot resets CLOCK_MONOTONIC below LastMonotonicNS -> prior conflict clears.
	rebootedGuest := fixture(t)
	rebootedGuest.offset = -15 * time.Second
	rebootedGuest.synchronized = true
	rebootedGuest.monotonicBase = int64(30 * time.Second)
	rebootOptions := Options{
		Sync:             true,
		Automatic:        true,
		RecentlyAdjusted: true,
		LatchedConflict:  true,
		LastAdjustedAt:   now.Add(-2 * time.Minute),
		LastMonotonicNS:  int64(10 * time.Minute),
	}
	result, err = Run(context.Background(), rebootedGuest, "ssh", nil, vm.ReadyMarker{}, rebootOptions)
	if err != nil || result.Action != "adjusted" {
		t.Fatalf("guest reboot kept stale conflict: result=%+v err=%v", result, err)
	}
}

func TestClockCancellationClosesBothPipes(t *testing.T) {
	guest := fixture(t)
	guest.hang = true
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := Run(ctx, guest, "ssh", nil, vm.ReadyMarker{}, Options{Sync: true})
	if err == nil || time.Since(started) > 3*time.Second {
		t.Fatalf("unbounded cancellation: %v", err)
	}
}

func TestClockDrainsTheFinalJSONNewline(t *testing.T) {
	guest := fixture(t)
	guest.splitNewline = true
	result, err := Run(context.Background(), guest, "ssh", nil, vm.ReadyMarker{}, Options{})
	if err != nil || result.Action != "checked" {
		t.Fatalf("successful check lost during SSH shutdown: %+v %v", result, err)
	}
}

func TestClockTransportFailureDoesNotWaitForUnreadStdin(t *testing.T) {
	guest := fixture(t)
	guest.exitEarly = true
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := Run(ctx, guest, "ssh", nil, vm.ReadyMarker{}, Options{})
	if err == nil || ctx.Err() != nil {
		t.Fatalf("SSH failure waited for the entire deadline: %v", err)
	}
}
