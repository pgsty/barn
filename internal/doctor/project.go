package doctor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pgsty/farrow/internal/state"
	"golang.org/x/sys/unix"
)

func availableBytes(pathname string) (uint64, string, error) {
	probe := pathname
	for {
		info, err := os.Lstat(probe)
		if err == nil {
			if !info.IsDir() {
				probe = filepath.Dir(probe)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return 0, "", err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return 0, "", err
		}
		probe = parent
	}
	var statistics unix.Statfs_t
	if err := unix.Statfs(probe, &statistics); err != nil {
		return 0, "", err
	}
	return uint64(statistics.Bavail) * uint64(statistics.Bsize), probe, nil
}

func (p Probe) deploymentChecks() []Check {
	dataRoot, err := state.ResolveDataRoot()
	if err != nil {
		return []Check{{Name: "data-root", Status: Error, Evidence: err.Error()}}
	}
	checks := make([]Check, 0, 5)
	store := state.Store{Root: dataRoot}
	deployment, stateErr := store.ReadDeployment()
	switch {
	case errors.Is(stateErr, os.ErrNotExist):
		checks = append(checks, Check{Name: "deployment", Status: OK, Evidence: "no deployment state yet; farrow up creates it"})
	case stateErr != nil:
		checks = append(checks, Check{Name: "deployment", Status: Error, Evidence: stateErr.Error(), Fix: "restore state.json from a backup; to start over, move " + dataRoot + " aside (node disks stay in it), then run farrow up"})
	default:
		checks = append(checks, Check{Name: "deployment", Status: OK, Evidence: fmt.Sprintf("%s (%d node(s))", deployment.Resolved.Name, len(deployment.Resolved.Nodes))})
		for _, definition := range deployment.Resolved.Nodes {
			node, nodeErr := store.ReadNode(definition.Name)
			checkName := "node-state/" + definition.Name
			if nodeErr != nil {
				checks = append(checks, Check{Name: checkName, Status: Error, Evidence: nodeErr.Error(), Fix: "restore the node state from a backup, or run farrow recreate --force " + definition.Name + " (replaces its root disk; persistent data disks stay)"})
				continue
			}
			checks = append(checks, Check{Name: checkName, Status: OK, Evidence: fmt.Sprintf("%s, ssh 127.0.0.1:%d", node.Phase, node.SSHPort)})
			if nodeDir, pathErr := store.NodeDir(definition.Name); pathErr != nil {
				checks = append(checks, Check{Name: "transaction/" + definition.Name, Status: Error, Evidence: pathErr.Error()})
			} else if info, journalErr := os.Lstat(filepath.Join(nodeDir, "private-prepare.json")); journalErr == nil {
				check := Check{Name: "transaction/" + definition.Name, Status: Warn, Evidence: "an interrupted create of this node is pending", Fix: "run farrow up; it finishes or rolls back the interrupted create"}
				if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
					journal := filepath.Join(nodeDir, "private-prepare.json")
					check = Check{Name: check.Name, Status: Error, Evidence: journal + " is not a regular mode-0600 file", Fix: "inspect " + journal + "; Farrow will not read it until it is a regular file with mode 0600"}
				}
				checks = append(checks, check)
			} else if !errors.Is(journalErr, os.ErrNotExist) {
				checks = append(checks, Check{Name: "transaction/" + definition.Name, Status: Error, Evidence: journalErr.Error()})
			}
		}
	}
	available, filesystemPath, spaceErr := availableBytes(dataRoot)
	if spaceErr != nil {
		checks = append(checks, Check{Name: "data-root", Status: Error, Evidence: fmt.Sprintf("%s: %v", dataRoot, spaceErr)})
	} else {
		status := OK
		fix := ""
		if available < 20<<30 {
			status = Warn
			fix = "free space or choose a safe absolute FARROW_HOME"
		}
		checks = append(checks, Check{Name: "data-root", Status: status, Evidence: fmt.Sprintf("%s (filesystem %s, %.1f GiB available)", dataRoot, filesystemPath, float64(available)/(1<<30)), Fix: fix})
	}
	return checks
}
