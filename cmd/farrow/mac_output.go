package main

import (
	"fmt"
	"github.com/pgsty/farrow/internal/macvm"
	"io"
	"text/tabwriter"
)

func macOutcome(value any) commandOutcome {
	return commandOutcome{payload: value, text: func(out, _ io.Writer) error {
		formatBytes := func(value int64) string {
			for _, unit := range []struct {
				bytes int64
				name  string
			}{{1 << 30, "GiB"}, {1 << 20, "MiB"}, {1 << 10, "KiB"}} {
				if value >= unit.bytes {
					if value%unit.bytes == 0 {
						return fmt.Sprintf("%d %s", value/unit.bytes, unit.name)
					}
					return fmt.Sprintf("%.2f %s", float64(value)/float64(unit.bytes), unit.name)
				}
			}
			return fmt.Sprintf("%d B", value)
		}
		allocationNote := "Allocated space counts filesystem blocks; shared blocks may be counted more than once. It is not exclusive physical usage."
		switch result := value.(type) {
		case macvm.Status:
			w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "NAME\tSTATE\tIP\tSSH\tUSER\tOS\tCPU\tMEMORY\tDISK CAPACITY\tALLOCATED")
			showAllocationNote := false
			for _, slot := range result.Slots {
				ip, user, osVersion := slot.IP, slot.User, slot.Version
				build := slot.Build
				if slot.ObservedVersion != "" {
					osVersion, build = slot.ObservedVersion, slot.ObservedBuild
				}
				if ip == "" {
					ip = "—"
				}
				if user == "" {
					user = "—"
				}
				if osVersion == "" {
					osVersion = "—"
				} else {
					osVersion += " (" + build + ")"
				}
				cpu, memory, capacity, allocated := "—", "—", "—", "—"
				if slot.InstanceID != "" {
					cpu = fmt.Sprintf("%d", slot.CPU)
					memory = formatBytes(slot.MemoryBytes)
					capacity = formatBytes(slot.DiskBytes)
					if slot.DiskAllocatedBytes != nil {
						allocated = formatBytes(*slot.DiskAllocatedBytes)
						showAllocationNote = true
					}
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", slot.Name, slot.State, ip, slot.SSH, user, osVersion, cpu, memory, capacity, allocated)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			textField(out, 10, "data", result.Root)
			for _, slot := range result.Slots {
				for _, detail := range []string{slot.RuntimeError, slot.SSHError, slot.LastError} {
					if detail != "" {
						textField(out, 10, slot.Name, detail)
					}
				}
			}
			if !result.Prepared {
				textField(out, 10, "create", "farrow mac up")
			}
			if showAllocationNote && verboseOutput(out) {
				_, err := fmt.Fprintln(out, allocationNote)
				return err
			}
			return nil
		case *macvm.Slot:
			_, err := fmt.Fprintf(out, "%s: %s\n", result.Name, result.State)
			return err
		case *macvm.BaseImage:
			bestEffortf(out, "  %s  macOS %s (%s) base ready\n", styled(out, ansiGreen, "✓"), result.Version, result.Build)
			textField(out, 10, "create", "farrow mac up")
			return nil
		case []macvm.ImageEntry:
			w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "KIND\tBUILD\tSTATE\tFILE SIZE\tALLOCATED\tDISK CAPACITY\tREFERENCES\tDEFAULT")
			for _, image := range result {
				capacity := "—"
				if image.VirtualCapacityBytes > 0 {
					capacity = formatBytes(image.VirtualCapacityBytes)
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%v\t%t\n", image.Kind, image.Build, image.State, formatBytes(image.SizeBytes), formatBytes(image.AllocatedBytes), capacity, image.References, image.Default)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			for _, image := range result {
				if image.Detail != "" {
					textField(out, 10, image.Kind+" "+image.Build, image.Detail)
				}
				if image.State == "damaged" && image.Kind == "installer" {
					textField(out, 10, "cleanup", "farrow mac image prune --installers")
				}
			}
			if len(result) > 0 && verboseOutput(out) {
				_, err := fmt.Fprintln(out, allocationNote)
				return err
			}
			return nil
		case macvm.DoctorReport:
			w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "CHECK\tRESULT\tDETAIL")
			for _, check := range result.Checks {
				state := "ok"
				if !check.OK {
					state = "fail"
				}
				// Keep multiline process diagnostics inside one table row.
				detail := []rune(check.Detail)
				for i, r := range detail {
					if r == '\n' || r == '\r' || r == '\t' {
						detail[i] = ' '
					}
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", check.Name, state, string(detail))
			}
			if err := w.Flush(); err != nil {
				return err
			}
			state := "ready"
			if !result.OK {
				state = "needs attention"
			}
			_, err := fmt.Fprintln(out, "Mac environment:", state)
			return err
		case map[string]any:
			if slot, ok := result["slot"].(string); ok && result["window"] == "open" {
				bestEffortf(out, "  %s  %s desktop opened · closing the window keeps it running\n", styled(out, ansiGreen, "✓"), slot)
				textField(out, 10, "shutdown", "farrow mac stop "+slot)
				return nil
			}
			if _, exists := result["removed"]; exists {
				removed, _ := result["removed"].([]string)
				bestEffortf(out, "  %s  %d cached images removed; referenced bases preserved\n", styled(out, ansiGreen, "✓"), len(removed))
				if verboseOutput(out) {
					for _, path := range removed {
						textField(out, 10, "removed", path)
					}
				}
				return nil
			}
			_, err := fmt.Fprintln(out, result)
			return err
		default:
			_, err := fmt.Fprintf(out, "%v\n", value)
			return err
		}
	}}
}

func macMessageOutcome(payload any, message string, next ...string) commandOutcome {
	return commandOutcome{payload: payload, text: func(out, _ io.Writer) error {
		bestEffortf(out, "  %s  %s\n", styled(out, ansiGreen, "✓"), message)
		for _, command := range next {
			textField(out, 10, "next", command)
		}
		return nil
	}}
}

func macSlotOutcome(m *macvm.Manager, slot *macvm.Slot, verb string) commandOutcome {
	return commandOutcome{payload: slot, text: func(out, _ io.Writer) error {
		if slot == nil {
			return nil
		}
		message := slot.Name + ": " + slot.State
		if verb == "configure" {
			message = fmt.Sprintf("%s: resources updated · %d CPUs · %s memory", slot.Name, slot.CPU, progressBytes(slot.MemoryBytes))
		}
		if verb == "destroy" {
			message = slot.Name + ": removed; shared images and network reservation preserved"
		}
		bestEffortf(out, "  %s  %s\n", styled(out, ansiGreen, "✓"), message)
		if slot.State == "ready" {
			if config, err := m.Store.LoadConfig(); err == nil {
				if pref, err := config.Preference(slot.Name); err == nil {
					textField(out, 10, "guest", slot.User+"@"+pref.IP)
				}
			}
			textField(out, 10, "shell", "farrow mac ssh "+slot.Name)
			textField(out, 10, "desktop", "farrow mac open "+slot.Name)
		} else if verb == "configure" || slot.State == "stopped" {
			if slot.Initialized {
				textField(out, 10, "start", "farrow mac start "+slot.Name)
			} else {
				textField(out, 10, "ready", "farrow mac up "+slot.Name)
			}
		} else if slot.State == "created" || slot.State == "provisioning" || slot.State == "running" {
			textField(out, 10, "ready", "farrow mac up "+slot.Name)
			if slot.State != "created" {
				textField(out, 10, "desktop", "farrow mac open "+slot.Name)
			}
		}
		return nil
	}}
}

func macNetworkOutcome(info macvm.NetworkInfo) commandOutcome {
	return commandOutcome{payload: info, text: func(out, _ io.Writer) error {
		status := "not installed"
		if info.Ready {
			status = "ready"
		} else if info.Installed {
			status = "needs attention"
		}
		if info.UpdateRequired {
			status += " · update available"
		}
		textField(out, 12, "network", status)
		if info.Subnet != "" {
			textField(out, 12, "subnet", info.Subnet)
		}
		if info.Diagnostic != "" {
			textField(out, 12, "detail", info.Diagnostic)
		}
		if verboseOutput(out) {
			textField(out, 12, "service", info.ServiceLabel)
			textField(out, 12, "config", info.ConfigPath)
			textField(out, 12, "log", info.LogPath)
		}
		if !info.Ready || info.UpdateRequired {
			textField(out, 12, "next", "farrow mac setup")
		}
		return nil
	}}
}
