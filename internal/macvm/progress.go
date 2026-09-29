package macvm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/pgsty/farrow/internal/activity"
)

// progressWriter turns the runner's timestamped JSONL progress on stderr into
// activity events. Unrecognized lines become plain messages.
type progressWriter struct {
	mu      sync.Mutex
	pending []byte
	report  activity.Reporter
}

// NewProgressWriter returns the runner progress sink for report.
func NewProgressWriter(report activity.Reporter) io.Writer {
	return &progressWriter{report: report}
}

func (w *progressWriter) Write(p []byte) (int, error) {
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
			w.report.Report(runnerEvent(line))
		}
	}
	if len(w.pending) > 64<<10 {
		w.pending = nil
	}
	return len(p), nil
}

func runnerEvent(line string) activity.Event {
	var native struct {
		Phase    string  `json:"phase"`
		Fraction float64 `json:"fraction"`
		Mode     string  `json:"mode"`
		Address  string  `json:"address"`
	}
	if json.Unmarshal([]byte(line), &native) != nil || native.Phase == "" {
		return activity.Event{Phase: "prepare", Message: line}
	}
	switch native.Phase {
	case "validating_image":
		return activity.Event{Phase: "image-verify", Message: "Checking the Apple restore image"}
	case "restoring":
		return activity.Event{Phase: "image-restore", Message: fmt.Sprintf("Installing macOS · %.0f%%", native.Fraction*100)}
	case "installation_completed", "stopping_restore_machine", "restore_machine_stopped":
		return activity.Event{Phase: "image-restore", Message: "Finishing the unbooted macOS base"}
	case "restored":
		return activity.Event{Phase: "image-restore", Message: "macOS base prepared", Done: true}
	case "network":
		return activity.Event{Phase: "network", Message: "Private network ready at " + native.Address}
	case "starting":
		return activity.Event{Phase: "guest-ready", Message: "Booting macOS"}
	case "running":
		return activity.Event{Phase: "guest-ready", Message: "macOS is running"}
	default:
		return activity.Event{Phase: "prepare", Message: strings.ReplaceAll(native.Phase, "_", " ")}
	}
}
