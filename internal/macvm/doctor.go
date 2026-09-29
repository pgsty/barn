package macvm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/pgsty/barn/internal/failure"
)

type DoctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	Next   string `json:"next,omitempty"`
}

type DoctorReport struct {
	OK     bool          `json:"ok"`
	Checks []DoctorCheck `json:"checks"`
}

func (report *DoctorReport) add(name string, err error, detail string) {
	check := DoctorCheck{Name: name, OK: err == nil, Detail: detail}
	if err != nil {
		report.OK = false
		check.Detail = err.Error()
		_, _, check.Next = failure.Classify(err)
	}
	report.Checks = append(report.Checks, check)
}

// Doctor checks the host, the Mac component, the data and every machine. It
// changes nothing and starts nothing.
func (m *Manager) Doctor(ctx context.Context) DoctorReport {
	report := DoctorReport{OK: true, Checks: []DoctorCheck{}}
	if err := m.requireRunner(ctx); err != nil {
		report.add("host", err, "")
		return report
	}
	signature, err := exec.CommandContext(ctx, "/usr/bin/codesign", "--verify", "--strict", m.Runner.Binary).CombinedOutput()
	if err != nil {
		err = fmt.Errorf("the Mac component signature did not verify: %w: %s", err, strings.TrimSpace(string(signature)))
	}
	report.add("component", err, m.Runner.Binary)
	var probe map[string]any
	err = m.Runner.Call(ctx, nil, &probe, "probe")
	if err == nil && probe["virtualization_supported"] != true {
		err = errors.New("this Mac cannot run virtual machines: Virtualization.framework reports it unsupported")
	}
	detail := "virtualization supported"
	if version, ok := probe["host_version"].(string); ok {
		detail = "macOS " + version + " on Apple Silicon; " + detail
	}
	report.add("virtualization", err, detail)
	free, err := availableBytes(m.Store.Root)
	if err == nil && free < 40<<30 {
		err = fmt.Errorf("%.1f GiB free; preparing macOS needs about 40 GiB", float64(free)/(1<<30))
	}
	report.add("disk", err, fmt.Sprintf("%.1f GiB free", float64(free)/(1<<30)))
	config, err := m.Store.LoadConfig()
	if errors.Is(err, os.ErrNotExist) {
		report.add("data", nil, "no Mac machines yet; barn mac up creates the first")
		return report
	}
	report.add("data", err, m.Store.Root)
	if err != nil {
		return report
	}
	if config.DefaultBaseID != "" {
		base, err := m.Store.LoadBase(config.DefaultBaseID)
		if err == nil {
			err = m.validateBase(base)
		}
		report.add("base", err, config.DefaultBaseID)
	}
	status, err := m.List(ctx, true)
	if err != nil {
		report.add("machines", err, "")
		return report
	}
	routes, routeErr := m.routes(ctx)
	for _, view := range status.Machines {
		machine := view.machine
		base, err := m.Store.LoadBase(machine.BaseID)
		if err == nil {
			err = m.validateBase(base)
		}
		report.add(machine.Name+"_base", err, machine.BaseID)
		if machine.Initialized {
			report.add(machine.Name+"_ssh_keys", m.checkSSHMaterial(machine), "private key and pinned host key present")
		}
		for _, share := range machine.Shares {
			report.add(machine.Name+"_share_"+share.Name, checkShareSource(share), share.Path)
		}
		switch view.State {
		case "running":
			var sshErr error
			if machine.Initialized && view.SSH != "ready" {
				sshErr = fmt.Errorf("%s is running but SSH is %s", machine.Name, view.SSH)
				for _, warning := range view.Warnings {
					sshErr = fmt.Errorf("%w: %s", sshErr, warning)
				}
			}
			report.add(machine.Name, sshErr, "running · SSH "+view.SSH+" · "+machine.Network.Address)
		case "stopped", "prepared":
			var networkErr error
			if routeErr != nil {
				networkErr = routeErr
			} else {
				networkErr = m.checkRoutesFree(machine, routes)
			}
			report.add(machine.Name, networkErr, view.State+" · network "+machine.Network.Subnet+" is free")
		default:
			err := fmt.Errorf("%s is %s", machine.Name, view.State)
			if view.Error != "" {
				err = fmt.Errorf("%w: %s", err, view.Error)
			}
			report.add(machine.Name, failure.WithNext(err, "barn mac logs "+machine.Name), "")
		}
	}
	return report
}

// runnerLogPath names a machine's runtime log for diagnostics.
func (m *Manager) runnerLogPath(name string) (string, error) {
	dir, err := m.Store.MachinePath(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "runner.log"), nil
}
