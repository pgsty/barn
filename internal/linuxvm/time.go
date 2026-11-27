package linuxvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pgsty/barn/internal/activity"
	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/fsutil"
	"github.com/pgsty/barn/internal/lock"
	"github.com/pgsty/barn/internal/state"
	"github.com/pgsty/barn/internal/timesync"
	"github.com/pgsty/barn/internal/vm"
)

type TimeNode struct {
	Name string `json:"name"`
	timesync.Result
	Error string `json:"error,omitempty"`
}

type TimeReport struct {
	Source    string     `json:"source"`
	Automatic bool       `json:"automatic"`
	Nodes     []TimeNode `json:"nodes"`
	Message   string     `json:"message,omitempty"`
}

type clockRecord struct {
	UUID        string    `json:"uuid"`
	AdjustedAt  time.Time `json:"adjusted_at"`
	MonotonicNS int64     `json:"monotonic_ns,omitempty"`
	Conflict    bool      `json:"conflict,omitempty"`
}

func pruneClockRecords(root string, removed []string, wholeDeployment bool) error {
	path := filepath.Join(root, "time-sync.json")
	if wholeDeployment {
		return removeOwnedRegularWithin(root, path)
	}
	records, err := readClockRecords(root)
	if err != nil || len(records) == 0 {
		return err
	}
	changed := false
	for _, name := range removed {
		if _, exists := records[name]; exists {
			delete(records, name)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if len(records) == 0 {
		return removeOwnedRegularWithin(root, path)
	}
	return fsutil.WriteJSONAtomic(path, records, 0o600)
}

func readClockRecords(root string) (map[string]clockRecord, error) {
	path := filepath.Join(root, "time-sync.json")
	records := make(map[string]clockRecord)
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Size() > 1<<20 {
			return nil, fmt.Errorf("time synchronization history is not a bounded regular file: %s", path)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		return records, nil
	} else {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read time synchronization history: %w", err)
	}
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("read time synchronization history: %w", err)
	}
	if records == nil {
		records = make(map[string]clockRecord)
	}
	return records, nil
}

func (m Manager) Time(ctx context.Context, options timesync.Options) (_ TimeReport, returnErr error) {
	report := TimeReport{Source: "host", Automatic: options.Automatic, Nodes: []TimeNode{}}
	deployment, err := m.openDeployment(false)
	if err != nil {
		return report, err
	}
	store := state.Store{Root: deployment.Root}
	if _, err := store.ReadDeployment(); err != nil {
		if options.SkipBusy && errors.Is(err, os.ErrNotExist) {
			report.Message = "no deployment"
			return report, nil
		}
		return report, fmt.Errorf("read deployment for time synchronization: %w", err)
	}
	var held *lock.File
	if options.SkipBusy {
		held, _, err = tryDeploymentLock(deployment.Root, false)
		if err == nil && held == nil {
			report.Message = "skipped: another barn command holds the deployment"
			return report, nil
		}
	} else {
		held, err = acquireDeploymentLock(ctx, deployment.Root, false, m.Progress)
	}
	if err != nil {
		return report, err
	}
	defer func() { returnErr = lock.JoinRelease(returnErr, held, "time synchronization lock") }()
	current, err := store.ReadDeployment()
	if err != nil {
		return report, err
	}
	names, err := selectedNodeNames(current.Resolved, m.Nodes)
	if err != nil {
		return report, err
	}
	// Converge exited or interrupted VMs under the exclusive lock before probing
	// QMP, so powered-off guests and post-reboot hosts skip cleanly instead of
	// failing every scheduled check with stale Running state.
	settle := make([]string, 0, len(names))
	for _, name := range names {
		if node, readErr := store.ReadNode(name); readErr == nil && (node.Phase == state.Running || node.Phase == state.Starting || node.Phase == state.Stopping || node.Phase == state.Destroying) {
			settle = append(settle, name)
		}
	}
	if len(settle) > 0 {
		settling := m
		settling.Nodes = settle
		var statusPartial *PartialError
		if _, settleErr := settling.statusForLocked(ctx, deployment, ""); settleErr != nil && !errors.As(settleErr, &statusPartial) {
			return report, settleErr
		}
	}
	records, err := readClockRecords(deployment.Root)
	if err != nil {
		return report, err
	}
	sshPath, err := m.lookPath("ssh")
	if err != nil {
		return report, err
	}
	report.Nodes = make([]TimeNode, len(names))
	uuid := make([]string, len(names))
	var group sync.WaitGroup
	slots := make(chan struct{}, 4)
	for index, name := range names {
		group.Go(func() {
			row := TimeNode{Name: name}
			defer func() {
				if row.Error != "" && row.Action == "" {
					row.Action = "failed"
				}
				report.Nodes[index] = row
			}()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				row.Error = ctx.Err().Error()
				return
			}
			node, err := store.ReadNode(name)
			if errors.Is(err, os.ErrNotExist) || err == nil && (node.Phase == state.Stopped || node.Phase == state.Prepared || node.Phase == state.Absent) {
				row.Action = "skipped-stopped"
				return
			}
			if err != nil {
				row.Error = err.Error()
				return
			}
			uuid[index] = node.VMUUID
			selected := m
			selected.Nodes = []string{name}
			nodeCtx, cancel := context.WithTimeout(ctx, timesync.Timeout)
			defer cancel()
			connections, err := selected.ConnectionsLocked(nodeCtx, deployment, held)
			if err != nil {
				row.Error = err.Error()
				return
			}
			connection := connections[0]
			expectedHash := node.GuestSpecHash
			if expectedHash == "" {
				expectedHash = node.SpecHash
			}
			expected := vm.ReadyMarker{Node: name, Generation: node.Generation, SpecHash: expectedHash}
			args := vm.SSHArgsForInstance(connection.User, connection.PrivateKey, connection.KnownHosts, connection.HostKeyAlias, connection.Port)
			if args == nil {
				row.Error = "invalid management SSH arguments"
				return
			}
			settings := options
			record := records[name]
			if record.UUID == node.VMUUID {
				age := time.Since(record.AdjustedAt)
				settings.RecentlyAdjusted = !record.AdjustedAt.IsZero() && age >= 0 && age < 10*time.Minute
				settings.LastAdjustedAt = record.AdjustedAt
				settings.LastMonotonicNS = record.MonotonicNS
				settings.LatchedConflict = record.Conflict
			}
			row.Result, err = timesync.Run(nodeCtx, m.runner(), sshPath, args, expected, settings)
			var transport *execx.CommandError
			if options.Automatic && row.Before == nil && errors.As(err, &transport) && transport.ExitCode == 255 && nodeCtx.Err() == nil {
				// Management SSH may lag host resume. Retry one transport failure
				// within the existing deadline, before any clock write was attempted.
				select {
				case <-time.After(time.Second):
					row.Result, err = timesync.Run(nodeCtx, m.runner(), sshPath, args, expected, settings)
				case <-nodeCtx.Done():
					err = nodeCtx.Err()
				}
			}
			if err != nil {
				row.Error = err.Error()
			}
		})
	}
	group.Wait()
	failures := []NodeFailure{}
	changed := false
	for index, row := range report.Nodes {
		if row.Error != "" {
			failures = append(failures, NodeFailure{Node: row.Name, Stage: "time-sync", Error: row.Error})
		}
		if !options.Sync {
			continue
		}
		switch row.Action {
		case "adjusted":
			adjustedAt := time.Now().UTC()
			var monotonic int64
			if row.After != nil {
				if !row.After.HostTime.IsZero() {
					adjustedAt = row.After.HostTime
				}
				monotonic = row.After.MonotonicNS
			}
			records[row.Name] = clockRecord{UUID: uuid[index], AdjustedAt: adjustedAt, MonotonicNS: monotonic}
			changed = true
		case "source-conflict":
			record := records[row.Name]
			record.UUID = uuid[index]
			record.Conflict = true
			records[row.Name] = record
			changed = true
		case "within-threshold":
			if record := records[row.Name]; record.Conflict {
				record.Conflict = false
				records[row.Name] = record
				changed = true
			}
		}
	}
	if changed {
		if err := fsutil.WriteJSONAtomic(filepath.Join(deployment.Root, "time-sync.json"), records, 0o600); err != nil {
			return report, fmt.Errorf("save time synchronization history: %w", err)
		}
	}
	if len(failures) > 0 {
		return report, newPartialError(failures, len(names))
	}
	return report, nil
}

func (m Manager) SyncGuestTime(ctx context.Context, nodes []string) error {
	if len(nodes) == 0 {
		return nil
	}
	m.Nodes = append([]string(nil), nodes...)
	m.Progress.Report(activity.Event{Phase: "time-sync", Message: "Checking guest clocks against the host"})
	report, err := m.Time(ctx, timesync.Options{Sync: true, Automatic: true})
	if err == nil && strings.HasPrefix(report.Message, "skipped:") {
		return errors.New(report.Message)
	}
	return err
}
