package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pgsty/barn/internal/activity"
)

const progressDelay = time.Second

type progress struct {
	stderr     io.Writer
	context    *outputContext
	parent     *progress
	summary    string
	operation  string
	started    time.Time
	cancel     context.CancelFunc
	done       chan struct{}
	once       sync.Once
	mu         sync.Mutex
	enabled    bool
	verbose    bool
	tty        bool
	stopped    bool
	stopErr    error
	suspended  int
	lines      int
	last       time.Time
	event      activity.Event
	nodes      map[string]activity.Event
	items      map[string]activity.Event
	itemOrder  []string
	activeItem string
	phase      string
	phaseAt    time.Time
	timings    map[string]time.Duration
	phaseOrder []string
}

func startProgress(parent context.Context, stderr io.Writer, message string) *progress {
	return startOperationProgress(parent, stderr, message, "")
}

func startLifecycleProgress(parent context.Context, stderr io.Writer, command string) *progress {
	return startOperationProgress(parent, stderr, lifecycleMessage(command), command)
}

func startOperationProgress(parent context.Context, stderr io.Writer, message, operation string) *progress {
	state := outputContextFrom(stderr)
	item := &progress{stderr: rawWriter(stderr), context: state, summary: message, operation: operation, started: time.Now(), done: make(chan struct{}), nodes: make(map[string]activity.Event)}
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
		state.finalProgress = false
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
		if previousNode, ok := item.nodes[event.Node]; ok && event.Address == "" {
			event.Address = previousNode.Address
		}
		item.nodes[event.Node] = event
	} else if event.Item != "" {
		if item.items == nil {
			item.items = make(map[string]activity.Event)
		}
		if _, exists := item.items[event.Item]; !exists {
			item.itemOrder = append(item.itemOrder, event.Item)
		}
		item.items[event.Item] = event
		if !event.Done {
			item.activeItem = event.Item
		} else if item.activeItem == event.Item {
			item.activeItem = ""
		}
	} else if item.activeItem != "" && event.Message != "" {
		row := item.items[item.activeItem]
		row.Message = event.Message
		switch event.Phase {
		case "image-download":
			row.State = "downloading"
		case "image-verify":
			row.State = "verifying"
		case "image-restore":
			row.State = "restoring"
		}
		item.items[item.activeItem] = row
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
	case phase == "stop" || phase == "destroy" || phase == "purge":
		return phase
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
	label := phaseLabel(event.Phase)
	if event.Node != "" {
		var completed, started int
		label, completed, started = item.nodeProgress()
		result := "ready"
		switch strings.TrimPrefix(label, "mac ") {
		case "provision", "resize":
			result = "complete"
		case "stop":
			result = "stopped"
		case "destroy":
			result = "removed"
		case "purge":
			result = "purged"
		default:
			if started == len(item.nodes) {
				completed, result = started, "started"
			}
		}
		message = fmt.Sprintf("%d/%d nodes %s", completed, len(item.nodes), result)
		if item.verbose {
			message += "; " + event.Node + ": " + event.State
		}
	}
	bestEffortf(item.stderr, "%s: %s (%s)\n", label, message, elapsedLabel(now.Sub(item.started)))
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
	lines := item.renderLive(now)
	for _, line := range lines {
		bestEffortf(item.stderr, "\r\x1b[2K%s\n", line)
	}
	item.lines = len(lines)
	item.last = now
}

// A table taller than the viewport cannot be redrawn with cursor-up without
// erasing unrelated scrollback. Keep progress compact until the final table.
func (item *progress) renderLive(now time.Time) []string {
	lines := item.render(now)
	if len(lines) < outputRows(item.context, item.stderr) {
		return lines
	}
	lines = lines[len(lines)-1:]
	if outputRows(item.context, item.stderr) > 2 {
		event := item.event
		name := event.Node
		if name == "" {
			name = event.Item
		}
		detail := strings.TrimSpace(name + " " + event.State + " " + event.Message)
		lines = append(lines, progressLine(detail, outputColumns(item.context, item.stderr)))
	}
	return lines
}

const (
	progressNodeMaxWidth    = 64
	progressMessageMinWidth = 18
)

func compactNodeState(state string) string {
	switch state {
	case "waiting for guest setup", "waiting for SSH":
		return "waiting"
	case "SSH pending":
		return "pending"
	case "prepare failed", "setup failed":
		return "failed"
	case "setup incomplete":
		return "incomplete"
	default:
		return state
	}
}

func nodeActivity(event activity.Event, final bool) string {
	if final && event.Node != "" && (event.State == "ready" || event.State == "limited") && !strings.HasPrefix(event.Message, "SSH ready: ") {
		return "SSH ready: barn ssh " + event.Node
	}
	message := strings.Join(strings.Fields(event.Message), " ")
	message = strings.TrimPrefix(message, event.Node+":")
	message = strings.TrimSpace(message)
	if message == "" {
		message = event.State
	}
	return message
}

func progressLine(line string, columns int) string {
	return fitDisplay(line, max(1, columns-1))
}

func (item *progress) progressStateCell(state, cell string) string {
	if item.context == nil || !item.context.color || !item.tty {
		return cell
	}
	writer := &outputWriter{Writer: item.stderr, context: item.context, stderr: true}
	return styled(writer, statusStyle(state), cell)
}

func (item *progress) progressHeader(line string, columns int) string {
	line = progressLine(line, columns)
	if item.context == nil || !item.context.color || !item.tty {
		return line
	}
	writer := &outputWriter{Writer: item.stderr, context: item.context, stderr: true}
	return styled(writer, ansiDim, line)
}

func (item *progress) renderNodes(columns int) []string {
	names := make([]string, 0, len(item.nodes))
	for name := range item.nodes {
		names = append(names, name)
	}
	sort.Strings(names)
	return item.renderRows(columns, names, item.nodes, "NAME", true)
}

func (item *progress) renderRows(columns int, names []string, events map[string]activity.Event, nameHeader string, addresses bool) []string {
	if len(names) == 0 {
		return nil
	}

	nameWidth, addressWidth := displayWidth(nameHeader), 0
	if addresses {
		addressWidth = displayWidth("ADDRESS")
	}
	stateWidth := displayWidth("STATE")
	for _, name := range names {
		node := events[name]
		address := strings.TrimSpace(node.Address)
		if address == "" {
			address = "-"
		}
		state := compactNodeState(strings.TrimSpace(node.State))
		if state == "" {
			state = "pending"
		}
		nameWidth = max(nameWidth, displayWidth(name))
		if addresses {
			addressWidth = max(addressWidth, displayWidth(address))
		}
		stateWidth = max(stateWidth, displayWidth(state))
	}
	nameWidth = min(progressNodeMaxWidth, nameWidth)
	gapCount := 1
	if addresses {
		gapCount++
	}
	columnGap := 2
	if nameWidth+addressWidth+stateWidth+columnGap*gapCount >= columns {
		columnGap = 1
	}
	maxNameForTerminal := columns - 1 - addressWidth - stateWidth - columnGap*gapCount
	if maxNameForTerminal >= displayWidth(nameHeader) {
		nameWidth = min(nameWidth, maxNameForTerminal)
	}
	baseWidth := nameWidth + addressWidth + stateWidth + columnGap*gapCount
	availableMessageWidth := columns - 1 - baseWidth - columnGap
	showMessage := availableMessageWidth >= progressMessageMinWidth
	messageWidth := max(0, availableMessageWidth)
	separator := strings.Repeat(" ", columnGap)
	header := padDisplay(nameHeader, nameWidth) + separator
	if addresses {
		header += padDisplay("ADDRESS", addressWidth) + separator
	}
	header += padDisplay("STATE", stateWidth)
	if showMessage {
		header += separator + padDisplay("MESSAGE", messageWidth)
	}
	lines := []string{item.progressHeader(header, columns)}
	for _, name := range names {
		node := events[name]
		address := strings.TrimSpace(node.Address)
		if address == "" {
			address = "-"
		}
		state := compactNodeState(strings.TrimSpace(node.State))
		if state == "" {
			state = "pending"
		}
		nameCell := padDisplay(fitDisplay(name, nameWidth), nameWidth)
		addressCell := padDisplay(fitDisplay(address, addressWidth), addressWidth)
		stateCell := padDisplay(fitDisplay(state, stateWidth), stateWidth)
		prefix := nameCell + separator
		if addresses {
			prefix += addressCell + separator
		}
		plainLine := prefix + stateCell
		if showMessage {
			plainLine += separator + padDisplay(fitDisplay(nodeActivity(node, item.stopped), messageWidth), messageWidth)
		}
		line := progressLine(plainLine, columns)
		if displayWidth(plainLine) < columns {
			line = prefix + item.progressStateCell(state, stateCell)
			if showMessage {
				line += separator + padDisplay(fitDisplay(nodeActivity(node, item.stopped), messageWidth), messageWidth)
			}
		}
		lines = append(lines, line)
	}
	return lines
}

// Count the command's final state, not an intermediate stop during destroy
// or restart. The root operation survives nested progress and cleanup phases.
func (item *progress) nodeProgress() (label string, completed, started int) {
	label = item.operation
	kind := strings.TrimPrefix(label, "mac ")
	if kind == "" || kind == "up" || kind == "start" {
		label = strings.TrimSuffix(label, kind) + "boot"
	}
	for _, node := range item.nodes {
		switch kind {
		case "provision":
			if node.State == "success" {
				completed++
			}
		case "resize":
			if node.State == "resized" || node.State == "unchanged" {
				completed++
			}
		case "stop":
			if node.State == "stopped" {
				completed++
			}
		case "destroy":
			if node.State == "removed" || node.State == "purged" {
				completed++
			}
		case "purge":
			if node.State == "purged" {
				completed++
			}
		default:
			if node.State == "ready" || node.State == "limited" {
				completed++
			}
			if node.State == "started" {
				started++
			}
		}
	}
	return label, completed, started
}

func (item *progress) render(now time.Time) []string {
	columns := outputColumns(item.context, item.stderr)
	event := compactActivity(item.event)
	message := event.Message
	if message == "" {
		message = item.summary
	}
	label := phaseLabel(event.Phase)
	showNodes := len(item.nodes) > 0
	showItems := !showNodes && len(item.items) > 0
	elapsed := elapsedLabel(now.Sub(item.started))
	line := ""
	if showNodes {
		label, completed, started := item.nodeProgress()
		line = fmt.Sprintf("%s %d/%d nodes: %s", label, completed, len(item.nodes), elapsed)
		if started == len(item.nodes) {
			line = fmt.Sprintf("%s %d/%d nodes: %s", label, started, len(item.nodes), elapsed)
		} else if started > 0 {
			line = fmt.Sprintf("%s %d/%d nodes: %s, %d started", label, completed, len(item.nodes), elapsed, started)
		}
		line = item.summaryLine(line, columns)
	} else if showItems {
		completed := 0
		for _, event := range item.items {
			if event.Done {
				completed++
			}
		}
		label := item.operation
		if label == "" {
			label = phaseLabel(event.Phase)
		}
		line = item.summaryLine(fmt.Sprintf("%s %d/%d items: %s", label, completed, len(item.items), elapsed), columns)
	} else {
		line = item.summaryLine(fmt.Sprintf("%s: %s (%s)", label, message, elapsed), columns)
	}
	var lines []string
	if showNodes {
		lines = append(lines, item.renderNodes(columns)...)
	} else if showItems {
		header := "ITEM"
		if strings.HasPrefix(item.operation, "image ") || strings.HasPrefix(item.operation, "repo ") {
			header = "IMAGE"
		}
		lines = append(lines, item.renderRows(columns, item.itemOrder, item.items, header, false)...)
	}
	if !item.stopped && (event.TotalBytes > 0 || event.CurrentBytes > 0) {
		metrics := event
		metrics.Message, metrics.Source = "", ""
		detail := "transfer: " + formatActivity(metrics, now)
		lines = append(lines, fitDisplay(detail, columns-1))
		if event.TotalBytes > 0 {
			lines = append(lines, progressBar(event.CurrentBytes, event.TotalBytes, min(terminalProgressBarWidth, columns-3), now.Sub(item.started)))
		}
		if event.Source != "" && columns >= 60 {
			lines = append(lines, fitDisplay("source: "+event.Source, columns-1))
		}
	}
	return append(lines, line)
}

func (item *progress) summaryLine(line string, columns int) string {
	marker := item.completionMarker()
	line = fitDisplay(line, max(0, columns-1-displayWidth(marker)))
	if !item.tty {
		return line + marker
	}
	writer := &outputWriter{Writer: item.stderr, context: item.context, stderr: true}
	return line + styledCompletionMarker(writer, marker)
}

func (item *progress) completionMarker() string {
	if !item.stopped {
		return ""
	}
	if errors.Is(item.stopErr, ErrCancelled) || errors.Is(item.stopErr, context.Canceled) {
		return " [CANCELLED]"
	}
	if item.stopErr != nil {
		return " [FAILED]"
	}
	return " [DONE]"
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
	item.lines = min(item.lines, max(0, outputRows(item.context, item.stderr)-1))
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
		item.stopErr = err
		item.recordPhase("", time.Now())
		item.clearTTY()
		freezeResult := item.tty && (len(item.nodes) > 0 || len(item.items) > 0)
		if freezeResult {
			for _, line := range item.render(time.Now()) {
				bestEffortf(item.stderr, "\r\x1b[2K%s\n", line)
			}
		}
		item.mu.Unlock()
		item.context.mu.Lock()
		if freezeResult {
			item.context.finalProgress = true
		}
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
