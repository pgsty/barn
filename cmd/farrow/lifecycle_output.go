package main

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/pgsty/farrow/internal/failure"
	"github.com/pgsty/farrow/internal/hostshare"
	privatevm "github.com/pgsty/farrow/internal/private"
	"github.com/pgsty/farrow/internal/state"
)

type lifecycleWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
	Next    string `json:"next,omitempty"`
}

type lifecycleRetryOptions struct {
	Action, Repository string
	NoWait, Rollback   bool
}

func sshIntegrationWarning(action string, err error) lifecycleWarning {
	message := "SSH aliases could not be updated; use farrow ssh to connect"
	if action == "remove" {
		message = "VM data removed; the old SSH aliases could not be removed"
	}
	next := "farrow ssh-config --" + action
	if _, _, hinted := failure.Classify(err); hinted != "" {
		next = hinted
	}
	return lifecycleWarning{Code: "ssh_config", Message: message, Detail: err.Error(), Next: next}
}

func startupCommand(command string) bool {
	switch command {
	case "up", "start", "restart", "reload", "recreate":
		return true
	default:
		return false
	}
}

func printLifecycleResult(out io.Writer, command string, result lifecycleResult, noWait bool) {
	for _, node := range result.Nodes {
		if node.Error == "" {
			continue
		}
		found := false
		for _, failure := range result.Failures {
			found = found || failure.Node == node.Name
		}
		if !found {
			result.Failures = append(result.Failures, privatevm.NodeFailure{Node: node.Name, Stage: "status", Error: node.Error})
		}
	}
	// Preparation can fail before a node has committed state. Keep it in the
	// denominator and final result without pretending it reached the runtime.
	known := make(map[string]bool)
	for _, node := range result.Nodes {
		known[node.Name] = true
	}
	for _, failure := range result.Failures {
		if !known[failure.Node] {
			result.Nodes = append(result.Nodes, privatevm.NodeStatus{Name: failure.Node, State: state.Absent, Error: failure.Error})
			known[failure.Node] = true
		}
	}
	if command == "status" || !startupCommand(command) || verboseOutput(out) {
		if len(result.Failures) > 0 {
			status := result.Status
			status.Message = ""
			printStatusTable(out, status, false)
		} else {
			printPrivateStatus(out, result.Status)
		}
	} else {
		ready, limited, unchecked := 0, 0, 0
		changed := false
		for _, node := range result.Nodes {
			changed = changed || len(node.Repairs) != 0
			if node.Ready && node.Error == "" {
				ready++
				if len(node.Warnings) > 0 {
					limited++
				}
			} else if node.State == state.Running && node.Error == "" {
				unchecked++
			}
		}
		summary := fmt.Sprintf("%d/%d nodes ready", ready, len(result.Nodes))
		noun := "nodes"
		if len(result.Nodes) == 1 {
			noun = "node"
		}
		style, marker := ansiYellow, "!"
		switch {
		case noWait && len(result.Failures) == 0:
			summary = fmt.Sprintf("%d %s started; SSH not checked (--no-wait)", len(result.Nodes), noun)
			style, marker = ansiCyan, "·"
		case allNodesReady(result.Status):
			summary = fmt.Sprintf("%d %s ready", ready, noun)
			style, marker = ansiGreen, "✓"
			if limited > 0 {
				summary += fmt.Sprintf(" · %d with limitations", limited)
				style, marker = ansiYellow, "!"
			} else if result.Message == "already running" && len(result.Warnings) == 0 && !changed {
				summary += " · unchanged"
			}
		default:
			if unchecked > 0 {
				summary += fmt.Sprintf(" · %d running, unchecked", unchecked)
			}
		}
		bestEffortf(out, "  %s  %s\n", styled(out, style, marker), summary)
		if len(result.Failures) > 0 {
			rows := make([][]string, 0, len(result.Nodes))
			for _, node := range result.Nodes {
				phase := string(node.State)
				if node.Ready {
					phase = "ready"
				} else if node.Error != "" && node.State == state.Running {
					phase = "SSH pending"
					for _, failure := range result.Failures {
						if failure.Node == node.Name && failure.Stage == "bootstrap" {
							phase = "setup failed"
						}
						if failure.Node == node.Name && failure.Stage == "guest-setup" {
							phase = "setup incomplete"
						}
					}
				}
				rows = append(rows, []string{node.Name, phase, node.Address})
			}
			printTable(out, []string{"NAME", "STATE", "ADDRESS"}, rows, 1)
		}
	}
	if len(result.Failures) > 0 {
		printNodeFailures(out, result.Source, result.Failures, result.retryOptions)
	}
	printGuestLimitations(out, result.Status, result.Source, result.retryOptions)
	for _, warning := range result.Warnings {
		bestEffortf(out, "  %s  %s\n", styled(out, ansiYellow, "!"), warning.Message)
		if verboseOutput(out) && warning.Detail != "" {
			textField(out, 10, "detail", warning.Detail)
		}
		if warning.Next != "" {
			textField(out, 10, "retry", warning.Next)
		}
	}
	if startupCommand(command) && (result.Message != "already running" || len(result.Warnings) > 0 || len(result.Failures) > 0) {
		for _, node := range result.Nodes {
			if node.Ready && node.Error == "" {
				textField(out, 10, "connect", "farrow ssh "+node.Name)
				break
			}
		}
	}
}

func printGuestLimitations(out io.Writer, status privatevm.Status, source string, options lifecycleRetryOptions) {
	groups := make(map[string][]string)
	var repairNode string
	for _, node := range status.Nodes {
		for _, repair := range node.Repairs {
			bestEffortf(out, "  ·  %s: %s\n", node.Name, repair)
		}
		if node.State != state.Running {
			continue
		}
		for _, warning := range node.Warnings {
			detail := strings.Join(strings.Fields(warning.Detail), " ")
			label := map[string]string{"data-disks": "data disks", "hosts": "guest hostnames", "shares": "shared directories", "control-ssh": "node-to-node SSH", "private-network": "private network", "warning-state": "saved status"}[warning.Stage]
			if label == "" {
				label = warning.Stage
			}
			message := label + ": " + detail
			groups[message] = append(groups[message], node.Name)
			needsKeyRestore := warning.Stage == "control-ssh" && strings.Contains(warning.Detail, "restore the original deployment private key")
			if repairNode == "" && warning.Stage != "warning-state" && !needsKeyRestore {
				repairNode = node.Name
			}
		}
	}
	messages := make([]string, 0, len(groups))
	for message := range groups {
		messages = append(messages, message)
	}
	sort.Strings(messages)
	for index, message := range messages {
		if index == 3 && !verboseOutput(out) {
			bestEffortf(out, "  !  %d more limitations; farrow status --verbose for details\n", len(messages)-index)
			break
		}
		names := groups[message]
		sort.Strings(names)
		bestEffortf(out, "  !  %s: %s\n", strings.Join(names, ", "), message)
	}
	if repairNode != "" {
		options.NoWait = false // Retrying guest setup requires a readiness check.
		textField(out, 10, "retry", lifecycleNodeCommand("up", source, []string{repairNode}, options))
	}
}

func printNodeFailures(out io.Writer, source string, failures []privatevm.NodeFailure, options ...lifecycleRetryOptions) {
	groups := make(map[string][]string)
	var retry, bootstrap, repair []string
	for _, failure := range failures {
		message := strings.Join(strings.Fields(failure.Error), " ")
		groups[message] = append(groups[message], failure.Node)
		if failure.Reason == hostshare.ReasonUnsupportedOnDarwin {
			// This host capability needs a configuration/host change. Repeating
			// up cannot recover it, and recreating a healthy root disk is not a
			// routine retry. Keep the explicit limitation, without either hint.
			continue
		}
		if failure.Stage == "bootstrap" {
			bootstrap = append(bootstrap, failure.Node)
		} else if failure.Stage == "guest-setup" {
			repair = append(repair, failure.Node)
		} else {
			retry = append(retry, failure.Node)
		}
	}
	messages := make([]string, 0, len(groups))
	for message := range groups {
		messages = append(messages, message)
	}
	sort.Strings(messages)
	for _, message := range messages {
		names := groups[message]
		sort.Strings(names)
		bestEffortf(out, "  !  %s: %s\n", strings.Join(names, ", "), message)
	}
	sort.Strings(retry)
	if len(repair) > 0 {
		sort.Strings(repair)
		repairOptions := lifecycleRetryOptions{}
		if len(options) > 0 {
			repairOptions = options[0]
		}
		repairOptions.NoWait = false
		textField(out, 10, "retry", lifecycleNodeCommand("up", source, repair, repairOptions))
		textField(out, 10, "logs", "farrow logs "+repair[0])
	}
	if len(retry) > 0 {
		textField(out, 10, "retry", lifecycleRetryCommand(source, retry, options...))
		textField(out, 10, "logs", "farrow logs "+retry[0])
	}
	if len(bootstrap) > 0 {
		sort.Strings(bootstrap)
		textField(out, 10, "logs", "farrow logs "+bootstrap[0])
		textField(out, 10, "inspect", "farrow ssh "+bootstrap[0])
		textField(out, 10, "rebuild", lifecycleNodeCommand("recreate", source, bootstrap, options...))
		bestEffortln(out, "           rebuild replaces root and non-persistent disks; inspect first")
	}
}

func lifecycleRetryCommand(source string, nodes []string, options ...lifecycleRetryOptions) string {
	action := "up"
	if len(options) != 0 && options[0].Action != "" {
		action = options[0].Action
	}
	return lifecycleNodeCommand(action, source, nodes, options...)
}

func lifecycleNodeCommand(action, source string, nodes []string, options ...lifecycleRetryOptions) string {
	command := "farrow " + action
	readsConfig := lifecycleReadsConfig(action)
	if readsConfig && source != "" && source != "applied deployment state" {
		command += " -f " + shellQuote(source)
	}
	if len(options) != 0 {
		option := options[0]
		if readsConfig && option.Repository != "" {
			command += " --repo " + shellQuote(option.Repository)
		}
		if startupCommand(action) && option.NoWait {
			command += " --no-wait"
		}
		if (action == "up" || action == "reload") && option.Rollback {
			command += " --rollback"
		}
	}
	for _, node := range nodes {
		command += " " + shellQuote(node)
	}
	return command
}
