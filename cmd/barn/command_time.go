package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pgsty/barn/internal/fsutil"
	"github.com/pgsty/barn/internal/linuxvm"
	"github.com/pgsty/barn/internal/state"
	"github.com/pgsty/barn/internal/timejob"
	"github.com/pgsty/barn/internal/timesync"
	"github.com/spf13/cobra"
)

type timeRun struct {
	linuxvm.TimeReport
	At    time.Time `json:"at"`
	Error string    `json:"error,omitempty"`
}

type timeAutoResult struct {
	timejob.Status
	LastRun *timeRun `json:"last_run,omitempty"`
}

func newTimeCommand(stdout, stderr io.Writer) *cobra.Command {
	command := &cobra.Command{Use: "time", Short: "Inspect and synchronize Linux guest clocks with the host", Long: `Compare Linux guest clocks with this host over management SSH.
Time sync immediately sets selected running guests to the host clock, forwards
or backwards. Automatic checks correct offsets above two seconds. Guest NTP
configuration and host time services are left to the administrator.`, Example: `  barn time status
  barn time sync
  barn time auto enable`}
	configureHelpOnly(command, "time requires status, sync, or auto", stdout, stderr)
	for _, name := range []string{"status", "sync"} {
		var timeout time.Duration
		var automatic bool
		child := &cobra.Command{Use: name + " [node...]", Short: map[string]string{"status": "Measure guest offsets and report NTP state", "sync": "Set running guest clocks to the host and verify the result"}[name], Long: map[string]string{
			"status": "Measure host-relative offsets, round-trip uncertainty and guest NTP state.\nOmitted node names select the entire applied deployment; stopped nodes are skipped.",
			"sync":   "Immediately set running Linux guest clocks to the host, forwards or backwards,\nthen verify the result. Omitted node names select the entire applied deployment.\nStopped nodes are skipped; unreachable nodes fail independently. Guest Python 3\nand passwordless sudo are required, as provided by official Barn images.",
		}[name], Example: "  barn time " + name + "\n  barn --json time " + name + " meta", Args: cobra.ArbitraryArgs, ValidArgsFunction: nodeCompletion(false, false), RunE: func(cmd *cobra.Command, nodes []string) (returnErr error) {
			if timeout < time.Second || timeout > 10*time.Minute {
				return newUsageError(errors.New("time --timeout must be between 1s and 10m"))
			}
			if !automatic {
				resolved, resolveErr := currentDeploymentResolved()
				if resolveErr != nil {
					return deploymentReadError(resolveErr)
				}
				if selectorErr := validateNodeSelectors(resolved, nodes, "deployment"); selectorErr != nil {
					return newUsageError(selectorErr)
				}
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			var progressItem *progress
			if !automatic {
				progressItem = startOperationProgress(ctx, stderr, "Checking guest clocks over management SSH", "time "+name)
				defer func() { progressItem.Stop(returnErr) }()
			}
			manager := linuxvm.Manager{Nodes: nodes, Progress: deferredProgressReporter(&progressItem)}
			report, err := manager.Time(ctx, timesync.Options{Sync: name == "sync", Automatic: automatic, SkipBusy: automatic})
			result := timeRun{TimeReport: report, At: time.Now().UTC()}
			if err != nil {
				boundary := newCommandError(exitRuntime, err)
				result.Error = boundary.failure.Error
				if linuxvm.IsolatedPartial(err) == nil {
					result.Message = err.Error()
				}
			}
			if automatic {
				if saveErr := saveTimeRun(result); saveErr != nil {
					err = errors.Join(err, saveErr)
					result.Error = newCommandError(exitRuntime, err).failure.Error
					result.Message = err.Error()
				}
			}
			outcome := commandOutcome{payload: result, text: func(writer, _ io.Writer) error { return renderTime(writer, result, automatic) }}
			if err != nil {
				if len(report.Nodes) == 0 {
					if !automatic && errors.Is(err, os.ErrNotExist) {
						return newConflictError(errNoDeployment)
					}
					return newCommandError(exitRuntime, err)
				}
				boundary := newCommandError(exitRuntime, err)
				if partial := linuxvm.IsolatedPartial(err); partial != nil {
					boundary.failure.Message = fmt.Sprintf("%d of %d node(s) failed", len(partial.Failures), partial.Total)
				}
				return boundary.withPayload(result).withText(outcome.text)
			}
			return collectCommandOutcome(cmd.Context(), outcome)
		}}
		child.Flags().DurationVar(&timeout, "timeout", time.Minute, "maximum time for all selected nodes")
		noFileCompletions(child, "timeout")
		if name == "sync" {
			child.Flags().BoolVar(&automatic, "automatic", false, "use the automatic drift threshold and source conflict check")
			if err := child.Flags().MarkHidden("automatic"); err != nil {
				panic(err)
			}
			noFileCompletions(child, "automatic")
		}
		command.AddCommand(child)
	}
	auto := &cobra.Command{Use: "auto", Short: "Manage the optional per-user time synchronization timer", Long: "Install an optional launchd user agent or systemd user timer. The OS starts\na bounded one-shot check every minute and catches missed calendar events after\nresume. Checks use the executable and BARN_HOME recorded at enable time.\nNo Barn process remains resident between checks.", Example: "  barn time auto enable\n  barn time auto status\n  barn time auto disable"}
	configureHelpOnly(auto, "time auto requires enable, disable, or status", stdout, stderr)
	for _, action := range []string{"enable", "disable", "status"} {
		child := &cobra.Command{Use: action, Short: map[string]string{"enable": "Install a check every minute and after host resume", "disable": "Stop and remove the scheduled check", "status": "Show timer registration and its latest result"}[action], Long: map[string]string{
			"enable":  "Install a per-user OS timer using this executable and BARN_HOME. Each check\nruns for at most 45 seconds, skips busy deployments, and corrects offsets above\ntwo seconds. Re-run enable after moving Barn. Use a persistent executable path.",
			"disable": "Stop the scheduled time check and remove its Barn-owned job files.\nManual synchronization and startup clock checks remain available.",
			"status":  "Show whether the OS timer is installed and active, its recorded BARN_HOME,\nand the latest scheduled check. The latest result replaces the previous one.",
		}[action], Example: "  barn time auto " + action, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) (returnErr error) {
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			root, err := state.ResolveDataRoot()
			if err != nil {
				return newRuntimeError(err)
			}
			manager, err := timejob.Current(root)
			if err != nil {
				return newRuntimeError(err)
			}
			var status timejob.Status
			if action == "status" {
				status, err = manager.Status(ctx)
			} else {
				summary := "Enabling the time synchronization timer"
				if action == "disable" {
					summary = "Disabling the time synchronization timer"
				}
				progressItem := startOperationProgress(ctx, stderr, summary, "time auto "+action)
				defer func() { progressItem.Stop(returnErr) }()
				status, err = manager.Change(ctx, action == "enable")
			}
			if err != nil {
				return newRuntimeError(err)
			}
			result := timeAutoResult{Status: status}
			if status.DataRoot != "" {
				result.LastRun, err = readTimeRun(status.DataRoot)
				if err != nil {
					return newRuntimeError(err)
				}
			}
			return collectCommandOutcome(cmd.Context(), commandOutcome{payload: result, text: func(writer, _ io.Writer) error {
				stateLabel := "inactive"
				summary := fmt.Sprintf("not installed (%s, every %ds)", status.Scheduler, status.IntervalSeconds)
				switch {
				case status.Installed && status.Active:
					stateLabel = "active"
					summary = fmt.Sprintf("%s, every %ds", status.Scheduler, status.IntervalSeconds)
				case status.Installed:
					stateLabel = "installed"
					summary = fmt.Sprintf("installed, scheduler inactive (%s)", status.Scheduler)
				}
				printItemResult(writer, "time-auto", stateLabel, summary)
				if status.DataRoot != "" {
					textField(writer, 10, "barn home", status.DataRoot)
				}
				if result.LastRun != nil {
					return renderTime(writer, *result.LastRun, false)
				}
				return outputWriteError(writer)
			}})
		}}
		auto.AddCommand(child)
	}
	command.AddCommand(auto)
	return command
}

func saveTimeRun(result timeRun) error {
	root, err := state.ResolveDataRoot()
	if err != nil {
		return err
	}
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWrite(filepath.Join(root, "time-last.json"), append(data, '\n'), 0o600)
}

func readTimeRun(root string) (*timeRun, error) {
	path := filepath.Join(root, "time-last.json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read latest time check: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, fmt.Errorf("latest time check is not a bounded regular file: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var result timeRun
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("read latest time check: %w", err)
	}
	return &result, nil
}

func formatTimeOffset(node linuxvm.TimeNode) string {
	if node.Before == nil {
		return "-"
	}
	if node.After == nil {
		return fmt.Sprintf("%+.3fms ±%.3fms", node.Before.OffsetMS, node.Before.UncertaintyMS)
	}
	return fmt.Sprintf("%+.3fms -> %+.3fms ±%.3fms", node.Before.OffsetMS, node.After.OffsetMS, node.After.UncertaintyMS)
}

func formatTimeNTP(node linuxvm.TimeNode) string {
	if node.Before == nil {
		return "-"
	}
	sync := "unknown"
	if node.NTP.Synchronized != nil {
		if *node.NTP.Synchronized {
			sync = "synced"
		} else {
			sync = "unsynced"
		}
	}
	detail := node.NTP.Service
	if source := strings.TrimSpace(node.NTP.Source); source != "" {
		if detail != "" && detail != "unknown" {
			detail += ", " + source
		} else {
			detail = source
		}
	}
	if detail == "" {
		return sync
	}
	return fmt.Sprintf("%s (%s)", sync, detail)
}

func formatTimeMessage(node linuxvm.TimeNode) string {
	if node.Error != "" {
		return node.Error
	}
	switch node.Action {
	case "adjusted":
		return "synchronized to host"
	case "within-threshold":
		return "within ±2s threshold"
	case "skipped-stopped":
		return "guest is stopped"
	case "checked":
		return "measured against host"
	default:
		return ""
	}
}

func renderTime(writer io.Writer, result timeRun, quiet bool) error {
	rows := make([][]string, 0, len(result.Nodes))
	for _, node := range result.Nodes {
		if quiet && node.Action != "adjusted" && node.Error == "" {
			continue
		}
		rows = append(rows, []string{node.Name, node.Action, formatTimeOffset(node), formatTimeNTP(node), formatTimeMessage(node)})
	}
	if quiet {
		if len(rows) > 0 {
			printTable(writer, []string{"NAME", "STATE", "OFFSET", "NTP", "MESSAGE"}, rows, 1)
		}
		return outputWriteError(writer)
	}
	textField(writer, 10, "reference", fmt.Sprintf("host clock at %s (positive = guest ahead)", result.At.Format(time.RFC3339)))
	if len(rows) > 0 {
		printTable(writer, []string{"NAME", "STATE", "OFFSET", "NTP", "MESSAGE"}, rows, 1)
	}
	if result.Message != "" {
		textField(writer, 10, "note", result.Message)
	}
	return outputWriteError(writer)
}
