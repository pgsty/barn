package macvm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type DoctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}
type DoctorReport struct {
	OK     bool          `json:"ok"`
	Checks []DoctorCheck `json:"checks"`
}

func (m *Manager) Doctor(ctx context.Context) DoctorReport {
	report := DoctorReport{OK: true, Checks: []DoctorCheck{}}
	if err := m.requireRunner(); err != nil {
		report.add("host_and_runner", err, "")
		return report
	}
	signature, err := exec.CommandContext(ctx, "/usr/bin/codesign", "--verify", "--strict", m.Runner.Binary).CombinedOutput()
	if err != nil {
		err = fmt.Errorf("runner signature verification failed: %w: %s", err, strings.TrimSpace(string(signature)))
	}
	report.add("runner_signature", err, "code signature verified (development signing is supported)")
	var probe map[string]any
	err = m.Runner.Call(ctx, nil, &probe, "probe")
	if err == nil && probe["virtualization_supported"] != true {
		err = fmt.Errorf("virtualization framework reports this host is unsupported")
	}
	hostDetail := "hardware virtualization is supported"
	if hostVersion, ok := probe["host_version"].(string); ok {
		hostDetail = "macOS " + hostVersion + " on Apple Silicon; " + hostDetail
	}
	report.add("virtualization", err, hostDetail)
	path := m.Store.Root
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		parent := filepath.Dir(path)
		if parent == path {
			break
		}
		path = parent
	}
	var disk unix.Statfs_t
	err = unix.Statfs(path, &disk)
	report.add("disk", err, fmt.Sprintf("%.1f GiB available", float64(disk.Bavail)*float64(disk.Bsize)/(1<<30)))
	config, err := m.Store.LoadConfig()
	if errors.Is(err, os.ErrNotExist) {
		err = errors.New("mac environment is not prepared; run farrow mac up")
	}
	report.add("configuration", err, "independent Mac state is valid")
	if err != nil {
		return report
	}
	network, err := m.networkStatus(ctx, config)
	report.add("network_helper", err, fmt.Sprintf("root helper pid %d; subnet %s", network.PID, network.Subnet))
	var verifiedNetwork *NetworkStatus
	if err == nil {
		verifiedNetwork = &network
	}
	report.add("network_routes", m.checkNetworkRoutes(ctx, config, verifiedNetwork), "saved subnet has no conflicting LAN, VPN or VM routes")
	slots, err := m.Store.ListSlots()
	report.add("slots", err, "mac1 and mac2 state is valid")
	if err != nil {
		return report
	}
	for _, slot := range slots {
		if slot.InstanceID == "" {
			continue
		}
		base, err := m.Store.LoadBase(slot.BaseID)
		report.add(slot.Name+"_base", err, slot.BaseID)
		if err != nil {
			continue
		}
		basePath, _ := m.Store.BasePath(base.ID)
		for _, name := range []string{"disk.asif", "hardware-model.bin", "auxiliary-storage.bin"} {
			_, err := os.Stat(filepath.Join(basePath, name))
			report.add(slot.Name+"_base_"+name, err, "present")
		}
		if slot.Initialized {
			report.add(slot.Name+"_ssh_material", m.checkSSHMaterial(&slot), "saved private key and host pin are valid")
		} else {
			report.add(slot.Name+"_initialization", fmt.Errorf("initialization incomplete; run farrow mac up %s", slot.Name), "")
		}
		pref, _ := config.Preference(slot.Name)
		view := SlotView{Slot: slot}
		m.inspectRuntime(ctx, &view, pref)
		report.addRuntime(view)
	}
	return report
}

func (report *DoctorReport) add(name string, err error, detail string) {
	check := DoctorCheck{Name: name, OK: err == nil, Detail: detail}
	if err != nil {
		report.OK = false
		check.Detail = err.Error()
	}
	report.Checks = append(report.Checks, check)
}

func (report *DoctorReport) addRuntime(view SlotView) {
	var runtimeErr error
	if view.RuntimeError != "" {
		runtimeErr = errors.New(view.RuntimeError)
	} else if view.State != "ready" && view.State != "running" && view.State != "stopped" && view.State != "created" {
		runtimeErr = fmt.Errorf("%s runtime is %s; inspect farrow mac logs %s", view.Name, view.State, view.Name)
	}
	report.add(view.Name+"_runtime", runtimeErr, view.State)
	if runtimeErr == nil && (view.State == "running" || view.State == "ready") && view.Initialized {
		var sshErr error
		if view.SSH != "ready" {
			sshErr = fmt.Errorf("%s SSH is %s: %s", view.Name, view.SSH, view.SSHError)
		}
		report.add(view.Name+"_ssh", sshErr, "pinned key authentication and passwordless sudo verified")
	}
}
