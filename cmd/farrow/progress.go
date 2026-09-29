package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pgsty/farrow/internal/activity"
)

const progressDelay = time.Second

type progress struct {
	stderr     io.Writer
	context    *outputContext
	parent     *progress
	summary    string
	started    time.Time
	cancel     context.CancelFunc
	done       chan struct{}
	once       sync.Once
	mu         sync.Mutex
	enabled    bool
	verbose    bool
	tty        bool
	stopped    bool
	suspended  int
	lines      int
	last       time.Time
	event      activity.Event
	nodes      map[string]activity.Event
	phase      string
	phaseAt    time.Time
	timings    map[string]time.Duration
	phaseOrder []string
}

func startProgress(parent context.Context, stderr io.Writer, message string) *progress {
	state := outputContextFrom(stderr)
	item := &progress{stderr: rawWriter(stderr), context: state, summary: message, started: time.Now(), done: make(chan struct{}), nodes: make(map[string]activity.Event)}
	item.enabled = state != nil && (state.stderrFile || state.verbose)
	if !item.enabled {
		close(item.done)
		return item
	}
	item.verbose = state.verbose
	if item.verbose {
		item.timings = make(map[string]time.Duration)
		item.recordPhase("host", item.started)
	}
	item.tty = state.stderrTTY && state.format == outputText && os.Getenv("TERM") != "dumb" && os.Getenv("CI") != "true" && os.Getenv("CI") != "1"
	state.mu.Lock()
	item.parent = state.active
	if item.parent == nil {
		state.active = item
	}
	state.mu.Unlock()
	if item.parent != nil {
		item.Report(activity.Event{Phase: "host", Message: message})
		close(item.done)
		return item
	}
	ctx, cancel := context.WithCancel(parent)
	item.cancel = cancel
	item.event = activity.Event{Message: message}
	go func() {
		defer close(item.done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				item.mu.Lock()
				item.repaint(now)
				item.mu.Unlock()
			}
		}
	}()
	return item
}

func (item *progress) Report(event activity.Event) {
	if item == nil || !item.enabled {
		return
	}
	if item.parent != nil {
		item.parent.Report(event)
		return
	}
	item.mu.Lock()
	defer item.mu.Unlock()
	if item.stopped {
		return
	}
	previous := item.event
	item.recordPhase(event.Phase, time.Now())
	item.event = event
	if event.Node != "" {
		item.nodes[event.Node] = event
	}
	// Plain logs retain meaningful milestones, while fast checks remain silent.
	// Waiting on another command is always worth one line, even in scripts.
	if !item.tty && (item.verbose || event.Phase == "lock") && (previous.Phase != event.Phase || event.Done) {
		item.logEvent(time.Now())
	}
}

func phaseLabel(phase string) string {
	switch {
	case strings.HasPrefix(phase, "image"):
		return "image"
	case phase == "guest-ready":
		return "boot"
	case phase == "stop":
		return "stop"
	case phase == "guest-metadata":
		return "guests"
	case phase == "ssh-config":
		return "ssh"
	case phase == "event-log":
		return "logs"
	case phase == "prepare" || phase == "commit":
		return "prepare"
	case strings.Contains(phase, "network"):
		return "network"
	default:
		return "host"
	}
}

func compactActivity(event activity.Event) activity.Event {
	if parsed, err := url.Parse(event.Source); err == nil && parsed.Host != "" {
		event.Source = parsed.Host
	}
	return event
}

func (item *progress) logEvent(now time.Time) {
	event := item.event
	if !item.verbose {
		event = compactActivity(event)
	}
	message := formatActivity(event, now)
	if event.Node != "" {
		ready := 0
		for _, node := range item.nodes {
			if node.State == "ready" || node.State == "limited" {
				ready++
			}
		}
		message = fmt.Sprintf("%d/%d nodes ready", ready, len(item.nodes))
		if item.verbose {
			message += "; " + event.Node + ": " + event.State
		}
	}
	bestEffortf(item.stderr, "  %s  %s (%s)\n", phaseLabel(event.Phase), message, elapsedLabel(now.Sub(item.started)))
	item.last = now
}

func (item *progress) repaint(now time.Time) {
	if item.stopped || item.suspended > 0 || now.Sub(item.started) < progressDelay {
		return
	}
	interval := 15 * time.Second
	if item.tty {
		interval = 250 * time.Millisecond
	}
	if !item.last.IsZero() && now.Sub(item.last) < interval {
		return
	}
	if !item.tty {
		item.logEvent(now)
		return
	}
	item.clearTTY()
	lines := item.render(now)
	for _, line := range lines {
		bestEffortf(item.stderr, "\r\x1b[2K%s\n", line)
	}
	item.lines = len(lines)
	item.last = now
}

func (item *progress) render(now time.Time) []string {
	columns := outputColumns(item.context, item.stderr)
	event := compactActivity(item.event)
	message := event.Message
	if message == "" {
		message = item.summary
	}
	label := phaseLabel(event.Phase)
	showNodes := len(item.nodes) > 0 && event.Phase == "guest-ready"
	ready := 0
	if showNodes {
		for _, node := range item.nodes {
			if node.State == "ready" || node.State == "limited" {
				ready++
			}
		}
		message = fmt.Sprintf("%d/%d nodes ready", ready, len(item.nodes))
	}
	elapsed := elapsedLabel(now.Sub(item.started))
	prefix := "  ·  " + padDisplay(label, 9) + " "
	line := prefix + fitDisplay(message, columns-displayWidth(prefix)-1)
	if columns >= 60 {
		line = prefix + padDisplay(fitDisplay(message, columns-displayWidth(prefix)-len(elapsed)-3), columns-displayWidth(prefix)-len(elapsed)-2) + " " + elapsed
	}
	lines := []string{line}
	if event.TotalBytes > 0 || event.CurrentBytes > 0 {
		metrics := event
		metrics.Message, metrics.Source = "", ""
		detail := strings.TrimPrefix(formatActivity(metrics, now), "— ")
		lines = append(lines, "     "+fitDisplay(detail, columns-6))
		if event.TotalBytes > 0 {
			lines = append(lines, "     "+progressBar(event.CurrentBytes, event.TotalBytes, min(terminalProgressBarWidth, columns-8), now.Sub(item.started)))
		}
		if event.Source != "" && columns >= 60 {
			lines = append(lines, "     "+fitDisplay(event.Source, columns-6))
		}
	}
	if showNodes {
		names := make([]string, 0, len(item.nodes))
		for name := range item.nodes {
			names = append(names, name)
		}
		sort.Strings(names)
		shown := 0
		for _, name := range names {
			node := item.nodes[name]
			if len(names) > 4 && (node.State == "ready" || node.State == "limited") {
				continue
			}
			if shown == 4 {
				break
			}
			lines = append(lines, "     "+fitDisplay(padDisplay(name, 16)+" "+node.State, columns-6))
			shown++
		}
		if len(names)-ready > shown {
			lines = append(lines, fmt.Sprintf("     %d more waiting", len(names)-ready-shown))
		}
	}
	if columns < 60 {
		lines = append(lines, "     elapsed   "+elapsed)
	}
	return lines
}

func elapsedLabel(duration time.Duration) string {
	if duration < time.Second {
		return "<1s"
	}
	return duration.Round(time.Second).String()
}

// Measure the wall time attributed to the active foreground phase. Repeated
// byte/node updates do not restart its clock, and nested progress reports use
// the root clock. This is elapsed time, including prompts, not CPU profiling.
func (item *progress) recordPhase(phase string, now time.Time) {
	if item.timings == nil || phase == item.phase {
		return
	}
	if item.phase != "" {
		item.timings[item.phase] += now.Sub(item.phaseAt)
	}
	item.phase, item.phaseAt = phase, now
	if phase != "" {
		if _, seen := item.timings[phase]; !seen {
			item.phaseOrder = append(item.phaseOrder, phase)
			item.timings[phase] = 0
		}
	}
}

func (item *progress) clearTTY() {
	for item.tty && item.lines > 0 {
		bestEffortf(item.stderr, "\x1b[1A\r\x1b[2K")
		item.lines--
	}
}

func (item *progress) writeExternal(data []byte) (int, error) {
	item.mu.Lock()
	defer item.mu.Unlock()
	item.clearTTY()
	return item.stderr.Write(data)
}

func suspendProgress(stderr io.Writer) func() {
	state := outputContextFrom(stderr)
	if state == nil {
		return func() {}
	}
	state.mu.Lock()
	item := state.active
	state.mu.Unlock()
	if item == nil {
		return func() {}
	}
	item.mu.Lock()
	item.suspended++
	item.clearTTY()
	item.mu.Unlock()
	return func() { item.mu.Lock(); item.suspended--; item.mu.Unlock() }
}

func (item *progress) Stop(err error) {
	if item == nil || !item.enabled || item.parent != nil {
		return
	}
	item.once.Do(func() {
		item.cancel()
		<-item.done
		item.mu.Lock()
		item.stopped = true
		item.recordPhase("", time.Now())
		item.clearTTY()
		item.mu.Unlock()
		item.context.mu.Lock()
		if item.context.active == item {
			item.context.active = nil
		}
		item.context.mu.Unlock()
		if item.verbose {
			writer := &outputWriter{Writer: item.stderr, context: item.context, stderr: true}
			for _, phase := range item.phaseOrder {
				debugf(writer, "timing %-18s %8s", phase, item.timings[phase].Round(time.Millisecond))
			}
			debugf(writer, "%s finished in %s (error=%v)", item.summary, time.Since(item.started).Round(time.Millisecond), err)
		}
	})
}

func tickf(stderr io.Writer, format string, arguments ...any) {
	debugf(stderr, format, arguments...)
}

func deferredProgressReporter(item **progress) activity.Reporter {
	return func(event activity.Event) {
		if item != nil && *item != nil {
			(*item).Report(event)
		}
	}
}
