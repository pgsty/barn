package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/pgsty/farrow/internal/activity"
	"github.com/spf13/cobra"
)

// Use the same delayed spinner, plain-log cadence and verbose timings as the
// Linux path. Native JSON progress is a transport detail, not terminal UI.
func wrapMacProgress(command *cobra.Command, stderr io.Writer) {
	if command.RunE != nil {
		switch command.Name() {
		case "setup", "init", "up", "start", "stop", "restart", "reset", "destroy", "configure", "update", "prune", "migrate", "uninstall":
			run := command.RunE
			command.RunE = func(cmd *cobra.Command, args []string) (err error) {
				item := startProgress(cmd.Context(), stderr, cmd.CommandPath())
				defer func() { item.Stop(err) }()
				return run(cmd, args)
			}
		}
	}
	for _, child := range command.Commands() {
		wrapMacProgress(child, stderr)
	}
}

type macProgressWriter struct {
	mu      sync.Mutex
	pending []byte
	item    *progress
}

func macProgressOutput(stderr io.Writer) io.Writer {
	state := outputContextFrom(stderr)
	if state == nil {
		return stderr
	}
	state.mu.Lock()
	item := state.active
	state.mu.Unlock()
	if item == nil {
		return stderr
	}
	return &macProgressWriter{item: item}
}

func (w *macProgressWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending = append(w.pending, p...)
	for {
		end := bytes.IndexByte(w.pending, '\n')
		if end < 0 {
			break
		}
		line := strings.TrimSpace(string(w.pending[:end]))
		w.pending = w.pending[end+1:]
		if line != "" {
			w.item.Report(macProgressEvent(line))
		}
	}
	if len(w.pending) > 65536 {
		w.pending = nil
	}
	return len(p), nil
}

func macProgressEvent(line string) activity.Event {
	e := activity.Event{Phase: "prepare", Message: line}
	var native struct {
		Phase    string  `json:"phase"`
		Fraction float64 `json:"fraction"`
	}
	if json.Unmarshal([]byte(line), &native) == nil && native.Phase != "" {
		switch native.Phase {
		case "restoring":
			e.Phase = "image-restore"
			e.Message = fmt.Sprintf("Installing macOS · %.0f%%", native.Fraction*100)
		case "validating_image":
			e.Phase = "image-verify"
			e.Message = "Checking the Apple restore image"
		case "installation_completed", "stopping_restore_machine", "restore_machine_stopped":
			e.Phase = "image-restore"
			e.Message = "Finishing the unbooted macOS base"
		case "restored":
			e.Phase = "image-restore"
			e.Message = "macOS base prepared"
			e.Done = true
		default:
			e.Message = strings.ReplaceAll(native.Phase, "_", " ")
		}
		return e
	}
	switch {
	case strings.HasPrefix(line, "Waiting for ") && strings.Contains(line, "Ctrl-C"):
		e.Phase = "lock"
	case strings.Contains(line, "SSH"):
		e.Phase = "guest-ready"
	case strings.HasPrefix(line, "IPSW:"):
		e.Phase = "image-download"
	case strings.Contains(strings.ToLower(line), "network"):
		e.Phase = "network"
	case strings.Contains(strings.ToLower(line), "base"):
		e.Phase = "image-prepare"
	}
	return e
}
