package darwin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/pgsty/barn/internal/network/subnet"
)

// Repair reuses the verified installed binaries and interface identity. It
// never downloads or replaces the host-global installation to restart it.
func (e Executor) Repair(ctx context.Context, mode, cidr string, restart bool) error {
	if err := e.validate(); err != nil {
		return err
	}
	plan, installed, err := e.readInstalled(ctx)
	if err != nil {
		return err
	}
	if !installed || plan.State.Mode != mode || plan.State.CIDR != cidr {
		return errors.New("network repair requires the matching installed mode and subnet")
	}
	if _, err := e.repairLogDirectory(ctx); err != nil {
		return err
	}
	layout, err := subnet.Parse(cidr)
	if err != nil {
		return err
	}
	before, err := e.snapshotExactHostInterfaces(ctx, layout)
	if err != nil {
		return err
	}
	if _, observed := before[plan.Interface.BSDName]; !restart && observed {
		// Permission repair alone must not disconnect running VMs. Only reuse
		// a socket with the expected ownership that actually accepts clients.
		if e.rootStat(ctx, SocketPath, "root", "staff", "770", "Socket") == nil {
			connection, dialErr := net.DialTimeout("unix", SocketPath, time.Second)
			if dialErr == nil {
				_ = connection.Close()
				return nil
			}
		}
	}
	delete(before, plan.Interface.BSDName)
	if err := e.restartService(ctx); err != nil {
		return err
	}
	if err := e.waitSocket(ctx); err != nil {
		return err
	}
	bsdName, err := e.waitNewExactHostInterface(ctx, layout, before)
	if err != nil {
		return err
	}
	plan, err = plan.WithBSDInterface(bsdName)
	if err != nil {
		return err
	}
	data, err := plan.InterfaceJSON()
	if err != nil {
		return err
	}
	staging, err := os.MkdirTemp(e.StagingParent, "barn-network-repair-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	source := filepath.Join(staging, "interface.json")
	if err := os.WriteFile(source, data, 0o600); err != nil {
		return err
	}
	for _, target := range []struct{ path, mode string }{{InterfaceStatePath, "0600"}, {InterfaceMarkerPath, "0644"}} {
		if _, err := e.Root.Run(ctx, "/usr/bin/install", "-o", "root", "-g", "wheel", "-m", target.mode, source, target.path); err != nil {
			return err
		}
	}
	verified, installed, err := e.readInstalled(ctx)
	if err != nil {
		return err
	}
	if !installed || verified.State != plan.State || verified.Interface != plan.Interface {
		return fmt.Errorf("network repair could not verify %s ownership", cidr)
	}
	return nil
}

// Called only after readInstalled has verified the root-owned installation.
func (e Executor) restartService(ctx context.Context) error {
	service := "system/" + ServiceID
	if _, err := e.Root.Run(ctx, "/bin/launchctl", "print", service); err != nil {
		if _, err := e.Root.Run(ctx, "/bin/launchctl", "bootstrap", "system", PlistPath); err != nil {
			return err
		}
	}
	if _, err := e.Root.Run(ctx, "/bin/launchctl", "enable", service); err != nil {
		return err
	}
	_, err := e.Root.Run(ctx, "/bin/launchctl", "kickstart", "-k", service)
	return err
}
