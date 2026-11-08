package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/pgsty/barn/internal/macvm"
)

func macCheck(out io.Writer) string { return styled(out, ansiGreen, "✓") }

func macBytes(value int64) string {
	if value >= 1<<30 && value%(1<<30) == 0 {
		return fmt.Sprintf("%d GiB", value>>30)
	}
	return progressBytes(value)
}

func macStatusOutcome(status macvm.Status) commandOutcome {
	return commandOutcome{payload: status, text: func(out, _ io.Writer) error {
		if len(status.Machines) == 0 {
			bestEffortln(out, "No Mac machines yet.")
			if status.Prepared {
				textField(out, 10, "base", fmt.Sprintf("macOS %s (%s) is prepared", status.Base.Version, status.Base.Build))
			}
			textField(out, 10, "create", "barn mac up")
			return nil
		}
		rows := make([][]string, 0, len(status.Machines))
		for _, view := range status.Machines {
			system := view.Image.Version
			if view.Observed != nil {
				system = view.Observed.Version
			}
			if system != "" {
				system = "macOS " + system
			}
			disk := macBytes(view.Disk.CapacityBytes)
			if view.Disk.AllocatedBytes != nil {
				disk = progressBytes(*view.Disk.AllocatedBytes) + " / " + disk
			}
			ssh := view.SSH
			if ssh == "unchecked" || ssh == "offline" {
				ssh = "—"
			}
			shares := []string{}
			for _, share := range view.Shares {
				shares = append(shares, share.Name)
			}
			rows = append(rows, []string{view.Name, view.Address, view.State, ssh, view.User, system, fmt.Sprintf("%d", view.CPUs), macBytes(view.MemoryBytes), disk, strings.Join(shares, ",")})
		}
		printTable(out, []string{"NAME", "ADDRESS", "STATE", "SSH", "USER", "OS", "CPU", "MEMORY", "DISK", "SHARED"}, rows, 2)
		for _, view := range status.Machines {
			if view.Error != "" {
				textField(out, 10, view.Name, view.Error)
			}
			for _, warning := range view.Warnings {
				textField(out, 10, view.Name, warning)
			}
		}
		if verboseOutput(out) {
			textField(out, 10, "data", status.Root)
		}
		if status.Running >= status.Limit {
			textField(out, 10, "limit", fmt.Sprintf("%d of %d macOS VMs are running; stop one before starting another", status.Running, status.Limit))
		}
		return nil
	}}
}

// macLifecycleOutcome renders one machine's result and its next commands.
func macLifecycleOutcome(outcome macvm.Outcome, stderr io.Writer) commandOutcome {
	return commandOutcome{payload: outcome, text: func(out, _ io.Writer) error {
		for _, warning := range outcome.Warnings {
			warningf(stderr, "%s", warning)
		}
		if !lifecycleProgressFrozen(out) {
			printTable(out, []string{"NAME", "ADDRESS", "STATE", "MESSAGE"}, [][]string{macOutcomeRow(outcome)}, 2)
		}
		switch {
		case outcome.Action == "configured":
			if outcome.State != "running" {
				textField(out, 10, "start", "barn mac start "+outcome.Name)
			}
		case outcome.Ready:
			if !outcome.Window {
				textField(out, 10, "desktop", "barn mac open "+outcome.Name)
			}
		case outcome.State == "running" && outcome.Action != "opened":
			textField(out, 10, "ready", "barn mac up "+outcome.Name)
			if !outcome.Window {
				textField(out, 10, "desktop", "barn mac open "+outcome.Name)
			}
		}
		return nil
	}}
}

func macOutcomeRow(outcome macvm.Outcome) []string {
	system := ""
	if outcome.Version != "" {
		system = fmt.Sprintf(" · macOS %s (%s)", outcome.Version, outcome.Build)
	}
	address := ""
	if outcome.Address != "" && outcome.User != "" {
		address = " · " + outcome.User + "@" + outcome.Address
	}
	var message string
	switch outcome.Action {
	case "created":
		message = outcome.Name + " created and ready" + system + address
		if !outcome.Ready {
			message = outcome.Name + " created and booting" + address
		}
	case "started", "restarted", "recreated", "running":
		verb := map[string]string{"started": "started", "restarted": "restarted", "recreated": "recreated", "running": "running"}[outcome.Action]
		if outcome.Ready {
			message = outcome.Name + " " + verb + " and ready" + system + address
		} else {
			message = outcome.Name + " " + verb + address
		}
	case "opened":
		message = outcome.Name + " desktop open · closing the window keeps it running"
	case "stopped":
		message = outcome.Name + " stopped"
		if outcome.Forced {
			message += " · powered off after the normal shutdown did not finish"
		}
	case "powered_off":
		message = outcome.Name + " powered off"
	case "already_stopped":
		message = outcome.Name + " was already stopped"
	case "destroyed":
		message = outcome.Name + " destroyed · the shared macOS base is kept"
	case "absent":
		message = outcome.Name + " does not exist"
	case "configured":
		message = outcome.Name + " configured"
	default:
		message = outcome.Name + " " + outcome.Action
	}
	phase := outcome.State
	if phase == "" {
		phase = outcome.Action
	}
	if outcome.Action == "destroyed" {
		phase = "removed"
	}
	message = strings.TrimPrefix(message, outcome.Name+" ")
	if outcome.Ready {
		phase = "ready"
		message = "SSH ready: barn mac ssh " + outcome.Name + system
	}
	ip := outcome.Address
	if ip == "" {
		ip = "—"
	}
	return []string{outcome.Name, ip, phase, message}
}

func macOutcomesOutcome(report macvm.Outcomes, stderr io.Writer) commandOutcome {
	return commandOutcome{payload: report, text: func(out, _ io.Writer) error {
		rows := make([][]string, 0, len(report.Machines))
		for _, outcome := range report.Machines {
			for _, warning := range outcome.Warnings {
				warningf(stderr, "%s", warning)
			}
			rows = append(rows, macOutcomeRow(outcome))
		}
		if !lifecycleProgressFrozen(out) {
			printTable(out, []string{"NAME", "ADDRESS", "STATE", "MESSAGE"}, rows, 2)
		}
		if len(report.Machines) == 1 {
			outcome := report.Machines[0]
			switch {
			case outcome.State == "stopped" || outcome.State == "prepared":
				textField(out, 10, "start", "barn mac start "+outcome.Name)
			}
		}
		return nil
	}}
}

func macMessageOutcome(payload any, message string, details ...string) commandOutcome {
	return commandOutcome{payload: payload, text: func(out, _ io.Writer) error {
		bestEffortf(out, "  %s  %s\n", macCheck(out), message)
		for _, detail := range details {
			textField(out, 10, "file", detail)
		}
		return nil
	}}
}

func macBaseOutcome(base *macvm.BaseImage) commandOutcome {
	return commandOutcome{payload: base, text: func(out, _ io.Writer) error {
		if base == nil {
			return nil
		}
		if !lifecycleProgressFrozen(out) {
			printTable(out, []string{"IMAGE", "STATE", "MESSAGE"}, [][]string{{"macOS", "ready", fmt.Sprintf("macOS %s (%s) is ready for new machines", base.Version, base.Build)}}, 1)
		}
		textField(out, 10, "create", "barn mac up")
		return nil
	}}
}

func macImagesOutcome(images []macvm.ImageEntry) commandOutcome {
	return commandOutcome{payload: images, text: func(out, _ io.Writer) error {
		if len(images) == 0 {
			bestEffortln(out, "No macOS images yet; barn mac up prepares one.")
			return nil
		}
		rows := make([][]string, 0, len(images))
		for _, image := range images {
			kind := map[string]string{"base": "base", "installer": "restore image"}[image.Kind]
			used := strings.Join(image.References, ",")
			if image.Default {
				used = strings.TrimPrefix(used+",default", ",")
			}
			capacity := "—"
			if image.VirtualCapacityBytes > 0 {
				capacity = macBytes(image.VirtualCapacityBytes)
			}
			rows = append(rows, []string{kind, "macOS " + image.Version, image.Build, image.State, progressBytes(image.AllocatedBytes), capacity, used})
		}
		printTable(out, []string{"KIND", "OS", "BUILD", "STATE", "ON DISK", "CAPACITY", "USED BY"}, rows, 3)
		for _, image := range images {
			if image.Detail != "" {
				textField(out, 10, image.Build, image.Detail)
			}
		}
		return nil
	}}
}

func macPruneOutcome(report macvm.PruneReport) commandOutcome {
	return commandOutcome{payload: report, text: func(out, _ io.Writer) error {
		if len(report.Candidates) == 0 {
			bestEffortf(out, "  %s  nothing to prune\n", macCheck(out))
			return nil
		}
		removed := make(map[string]bool)
		for _, entry := range report.Removed {
			removed[entry] = true
		}
		rows := make([][]string, 0, len(report.Candidates))
		for _, entry := range report.Candidates {
			phase := "planned"
			if report.Applied {
				phase = "kept"
				if removed[entry.Path] {
					phase = "removed"
				}
			}
			rows = append(rows, []string{entry.Kind + "/" + entry.Build, phase, progressBytes(entry.AllocatedBytes)})
		}
		printTable(out, []string{"IMAGE", "STATE", "SIZE"}, rows, 1)
		if !report.Applied {
			textField(out, 10, "reclaim", progressBytes(report.ReclaimedBytes))
			textField(out, 10, "apply", "rerun with --yes")
			return nil
		}
		bestEffortf(out, "  %s  removed %d of %d · %s reclaimed\n", macCheck(out), len(report.Removed), len(report.Candidates), progressBytes(report.ReclaimedBytes))
		return nil
	}}
}

func macDoctorOutcome(report macvm.DoctorReport) commandOutcome {
	return commandOutcome{payload: report, text: func(out, _ io.Writer) error {
		rows := make([][]string, 0, len(report.Checks))
		for _, check := range report.Checks {
			state := "ok"
			if !check.OK {
				state = "failed"
			}
			detail := strings.Join(strings.Fields(check.Detail), " ")
			rows = append(rows, []string{check.Name, state, detail})
		}
		if !lifecycleProgressFrozen(out) {
			printTable(out, []string{"ITEM", "STATE", "MESSAGE"}, rows, 1)
		}
		for _, check := range report.Checks {
			if !check.OK {
				textField(out, 10, "detail", check.Name+": "+check.Detail)
			}
			if !check.OK && check.Next != "" {
				textField(out, 10, "next", check.Next)
			}
		}
		return nil
	}}
}

// printMacHint mentions Mac machines under the Linux status. It reads only
// local records, never the machines, and stays silent on any error.
func printMacHint(out io.Writer) {
	if !macSupported() {
		return
	}
	store, err := macStore()
	if err != nil {
		return
	}
	machines, err := store.ListMachines()
	if err != nil || len(machines) == 0 {
		return
	}
	names := make([]string, 0, len(machines))
	for _, machine := range machines {
		names = append(names, machine.Name)
	}
	textField(out, 10, "mac", strings.Join(names, ", ")+" · barn mac ls")
}
