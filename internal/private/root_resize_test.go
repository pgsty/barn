package private

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/qemu"
	"github.com/pgsty/barn/internal/spec"
	"github.com/pgsty/barn/internal/state"
)

func rootResizeFixture() state.RootResize {
	return state.RootResize{Original: 8 * spec.GiB, Target: 10 * spec.GiB, Stage: "preflight", Guest: state.RootFilesystem{Serial: "serial", Partition: 1, Start: 1 << 20, UUID: "uuid", PartUUID: "part", Filesystem: "ext4", DiskBytes: 8 * spec.GiB, PartBytes: 8*spec.GiB - 2<<20, FSBytes: 8*spec.GiB - 2<<20}}
}

func TestRootResizeRecoveryAtEveryBoundary(t *testing.T) {
	// Lose the response both before and after every durable side effect,
	// including image growth, guest growth and final journal removal.
	for _, wasRunning := range []bool{false, true} {
		for _, after := range []bool{false, true} {
			for failAt := 1; failAt <= 15; failAt++ {
				t.Run(fmt.Sprintf("running=%t/after=%t/step=%d", wasRunning, after, failAt), func(t *testing.T) {
					initial := rootResizeFixture()
					initial.WasRunning = wasRunning
					persisted := &initial
					running, imageBytes, fsBytes, committed := wasRunning, initial.Original, initial.Guest.FSBytes, initial.Original
					steps, growCalls, fsCalls := 0, 0, 0
					fault := errors.New("lost response")
					operation := func(change func()) error {
						steps++
						if steps == failAt && !after {
							return fault
						}
						change()
						if steps == failAt {
							return fault
						}
						return nil
					}
					actions := rootResizeActions{
						save: func(j *state.RootResize) error {
							return operation(func() {
								if j == nil {
									persisted = nil
								} else {
									copy := *j
									persisted = &copy
								}
							})
						},
						start: func() error { return operation(func() { running = true }) },
						stop:  func() error { return operation(func() { running = false }) },
						probe: func(mode string, expected state.RootFilesystem) (state.RootFilesystem, error) {
							if !running {
								t.Fatal("guest probe while stopped")
							}
							actual := initial.Guest
							err := operation(func() {
								if mode == "grow" {
									if imageBytes != initial.Target {
										t.Fatal("guest growth before image growth")
									}
									if fsBytes == initial.Guest.FSBytes {
										fsCalls++
									}
									fsBytes = initial.Target - 2<<20
								}
								actual.DiskBytes = imageBytes
								actual.FSBytes = fsBytes
							})
							return actual, err
						},
						grow: func() error {
							if running {
								t.Fatal("image mutation while running")
							}
							return operation(func() {
								if imageBytes != initial.Target {
									growCalls++
								}
								imageBytes = initial.Target
							})
						},
						commit: func() error { return operation(func() { committed = imageBytes }) },
					}
					err := runRootResize(*persisted, actions)
					if err != nil && !errors.Is(err, fault) {
						t.Fatal(err)
					}
					if persisted != nil {
						steps = 100
						if err := runRootResize(*persisted, actions); err != nil {
							t.Fatal(err)
						}
					}
					if persisted != nil || imageBytes != initial.Target || committed != initial.Target || fsBytes <= initial.Guest.FSBytes || running != wasRunning || growCalls != 1 || fsCalls != 1 {
						t.Fatalf("journal=%+v image=%d applied=%d fs=%d running=%t writes=%d/%d", persisted, imageBytes, committed, fsBytes, running, growCalls, fsCalls)
					}
				})
			}
		}
	}
}

func TestResizeRootClassificationAndPendingTarget(t *testing.T) {
	current := singlePrivateResolved()
	desired := cloneResolved(current)
	desired.Nodes[0].RootDisk += spec.GiB
	diff := diffResolved(current, desired, func(string) bool { return true })
	if len(diff.Resize) != 1 || len(diff.Changed) != 0 {
		t.Fatalf("grow classification: %+v", diff)
	}
	store := state.Store{Root: t.TempDir()}
	node := resizeNodeState(t, current, store.Root)
	if err := store.WriteNode(node); err != nil {
		t.Fatal(err)
	}
	desired.Nodes[0].RootDisk = current.Nodes[0].RootDisk - spec.GiB
	if _, _, err := resizeTargets(current, desired, nil, store); err == nil || !strings.Contains(err.Error(), "cannot shrink") {
		t.Fatalf("shrink: %v", err)
	}
	node.RootResize = &state.RootResize{Original: current.Nodes[0].RootDisk, Target: current.Nodes[0].RootDisk + spec.GiB, Stage: "preflight"}
	if err := store.WriteNode(node); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resizeTargets(current, current, nil, store); err == nil || !strings.Contains(err.Error(), "pending") {
		t.Fatalf("pending target lost: %v", err)
	}
	desired.Nodes[0].RootDisk = node.RootResize.Target
	if _, names, err := resizeTargets(current, desired, nil, store); err != nil || len(names) != 1 {
		t.Fatalf("recovery target: %v %v", names, err)
	}
}

func TestRootGuestRejectsIncompleteGrowthAndIdentityChanges(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is unavailable")
	}
	script := rootResizeGuest + `
old = dict(serial="disk", partition=1, start=1048576, uuid="fs", part_uuid="part", filesystem="ext4", disk_bytes=8*1024**3, part_bytes=8*1024**3-2*1024**2, fs_bytes=8*1024**3-2*1024**2)
target = 10*1024**3
actual = dict(old, disk_bytes=target, part_bytes=target-2*1024**2, fs_bytes=target-2*1024**2)
verify(actual, old, target)
for key, value in [("serial", "other"), ("uuid", "other"), ("part_uuid", "other"), ("partition", 2), ("start", 2097152), ("disk_bytes", target-512), ("part_bytes", old["part_bytes"]), ("fs_bytes", old["fs_bytes"])]:
    try:
        verify(dict(actual, **{key: value}), old, target)
    except RuntimeError:
        pass
    else:
        raise AssertionError("accepted invalid " + key)
verify(dict(actual, filesystem="xfs", fs_bytes=actual["fs_bytes"]-1048576), dict(old, filesystem="xfs"), target)
`
	// Import semantics skip the command entry point while exercising the exact
	// shipped geometry checks against independent before/after measurements.
	cmd := exec.Command(python, "-c", "import sys; exec(sys.stdin.read(), {'__name__':'root_resize_test'})")
	cmd.Stdin = strings.NewReader(script)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("guest geometry checks: %v\n%s", err, output)
	}
}

func TestRootResizeKeepsPendingPeerOutsideSelection(t *testing.T) {
	current := diffFixtureResolved()
	store := state.Store{Root: t.TempDir()}
	for _, definition := range current.Nodes {
		node := resizeNodeState(t, current, store.Root)
		node.Node = definition.Name
		if definition.Name != "meta" {
			node.RootResize = &state.RootResize{Original: definition.RootDisk, Target: definition.RootDisk + spec.GiB, Stage: "preflight"}
		}
		if err := store.WriteNode(node); err != nil {
			t.Fatal(err)
		}
	}
	desired := cloneResolved(current)
	desired.Nodes[0].Memory += spec.GiB
	if _, names, err := resizeTargets(current, desired, []string{"meta"}, store); err != nil || len(names) != 1 || names[0] != "meta" {
		t.Fatalf("selected resize blocked by pending peer: %v %v", names, err)
	}
}

func TestPlanShowsUnfinishedRootAfterCapacityCommit(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BARN_HOME", root)
	resolved := singlePrivateResolved()
	store := state.Store{Root: root}
	if err := store.WriteDeployment(deploymentStateFor(t, resolved)); err != nil {
		t.Fatal(err)
	}
	node := resizeNodeState(t, resolved, root)
	node.Invocation = qemu.Invocation{Binary: "/fixture/qemu-system-aarch64", Args: []string{"-accel", "hvf"}}
	journal := rootResizeFixture()
	journal.Original = resolved.Nodes[0].RootDisk - spec.GiB
	journal.Target = resolved.Nodes[0].RootDisk
	node.RootResize = &journal
	if err := store.WriteNode(node); err != nil {
		t.Fatal(err)
	}
	plan, err := planFixtureManager(t).Plan(context.Background(), resolved)
	if err != nil || plan.Action != "resize" || len(plan.Resize) != 1 || plan.Destructive {
		t.Fatalf("pending root hidden: %+v %v", plan, err)
	}
}

func TestRootPreflightCanBeDiscardedOnlyBeforeMutationAndAfterPowerRestore(t *testing.T) {
	for _, running := range []bool{false, true} {
		for _, stage := range []string{"preflight", "image", "guest", "verified"} {
			for _, phase := range []state.Phase{state.Stopped, state.Running, state.Starting, state.Stopping} {
				node := state.NodeState{Phase: phase, RootResize: &state.RootResize{Stage: stage, WasRunning: running}}
				want := stage == "preflight" && (running && phase == state.Running || !running && phase == state.Stopped)
				if got := rootPreflightCanBeDiscarded(node); got != want {
					t.Fatalf("running=%t stage=%s phase=%s: got %t", running, stage, phase, got)
				}
			}
		}
	}
}
