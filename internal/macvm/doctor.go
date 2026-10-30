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

// setupDiskRequirement estimates the largest additional space the next
// default setup may need. A ready base needs no restore space, a cached
// installer needs only the restore workspace, and an interrupted download
// needs the remaining installer bytes plus that workspace.
func (m *Manager) setupDiskRequirement(config *Config) (int64, string, error) {
	if config != nil && config.DefaultBaseID != "" {
		if base, err := m.Store.LoadBase(config.DefaultBaseID); err == nil && base.State == "ready" && m.validateBase(base) == nil {
			return 0, "a verified base is ready; no restore space is required", nil
		}
	} else if base := m.readyBaseFor(DefaultIPSW().Build, DefaultDiskBytes); base != nil {
		return 0, "a verified default base is ready; no restore space is required", nil
	}

	spec := DefaultIPSW()
	images, err := m.Store.ImageList()
	if err != nil {
		return 0, "", err
	}
	for _, image := range images {
		if image.Kind == "installer" && image.ID == spec.Build && image.State == "ready" && image.SizeBytes == spec.SizeBytes {
			return macRestoreBytes, fmt.Sprintf("a cached installer is available; restore needs about %.1f GiB", float64(macRestoreBytes)/(1<<30)), nil
		}
	}
	resume, err := m.Store.PartialBytes(spec)
	if err != nil {
		return 0, "", err
	}
	needed := spec.SizeBytes - resume + macRestoreBytes
	return needed, fmt.Sprintf("no reusable base or installer; setup may need about %.1f GiB", float64(needed)/(1<<30)), nil
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
	config, configErr := m.Store.LoadConfig()
	if errors.Is(configErr, os.ErrNotExist) {
		config = nil
		configErr = nil
	}
	free, freeErr := availableBytes(m.Store.Root)
	needed, requirementDetail, requirementErr := m.setupDiskRequirement(config)
	diskErr := errors.Join(freeErr, requirementErr)
	if diskErr == nil && free < needed {
		diskErr = fmt.Errorf("%.1f GiB free; the next macOS setup may need about %.1f GiB", float64(free)/(1<<30), float64(needed)/(1<<30))
	}
	diskDetail := fmt.Sprintf("%.1f GiB free", float64(free)/(1<<30))
	if requirementDetail != "" {
		diskDetail += "; " + requirementDetail
	}
	report.add("disk", diskErr, diskDetail)
	if configErr != nil {
		report.add("data", configErr, m.Store.Root)
		return report
	}
	if config == nil {
		report.add("data", nil, "no Mac machines yet; barn mac up creates the first")
		return report
	}
	report.add("data", nil, m.Store.Root)
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
