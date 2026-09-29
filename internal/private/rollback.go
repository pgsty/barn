package private

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pgsty/barn/internal/fsutil"
)

type RollbackAction struct {
	Path    string `json:"path"`
	Kind    string `json:"kind"`
	Applied bool   `json:"applied"`
}

type RollbackResult struct {
	Node    string           `json:"node"`
	Apply   bool             `json:"apply"`
	Actions []RollbackAction `json:"actions"`
}

func invocationRuntime(journal PrepareJournal) (string, string, error) {
	qmpPath := ""
	pidfile := ""
	for index := 0; index < len(journal.Invocation.Args); index++ {
		switch journal.Invocation.Args[index] {
		case "-qmp":
			if index+1 >= len(journal.Invocation.Args) || !strings.HasPrefix(journal.Invocation.Args[index+1], "unix:") {
				return "", "", errors.New("invocation QMP argument is malformed")
			}
			qmpPath = strings.SplitN(strings.TrimPrefix(journal.Invocation.Args[index+1], "unix:"), ",", 2)[0]
		case "-pidfile":
			if index+1 >= len(journal.Invocation.Args) {
				return "", "", errors.New("invocation pidfile argument is malformed")
			}
			pidfile = journal.Invocation.Args[index+1]
		}
	}
	if journal.Prepared && (!filepath.IsAbs(qmpPath) || !filepath.IsAbs(pidfile) || filepath.Dir(qmpPath) != filepath.Dir(pidfile)) {
		return "", "", errors.New("invocation runtime paths are incomplete")
	}
	return qmpPath, pidfile, nil
}

func RollbackPrepared(deploymentValue Deployment, node string, apply bool) (RollbackResult, error) {
	nodeDir, err := deploymentValue.NodeDir(node)
	if err != nil {
		return RollbackResult{}, err
	}
	result := RollbackResult{Node: node, Apply: apply, Actions: []RollbackAction{}}
	journalPath := filepath.Join(nodeDir, "private-prepare.json")
	journal, err := ReadPrepareJournal(journalPath)
	if err != nil {
		return result, err
	}
	if journal.Node != node {
		return result, errors.New("rollback journal identity differs from the node")
	}
	if journal.StateCommitted {
		return result, errors.New("will not roll back: the node state is already committed")
	}
	if _, err := os.Lstat(filepath.Join(nodeDir, "state.json")); err == nil {
		return result, errors.New("will not roll back while node state exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	qmpPath, pidfile, err := invocationRuntime(journal)
	if err != nil {
		return result, err
	}
	for _, runtimePath := range []string{qmpPath, pidfile} {
		if runtimePath == "" {
			continue
		}
		if _, err := os.Lstat(runtimePath); err == nil {
			return result, fmt.Errorf("will not roll back while runtime file %s exists", runtimePath)
		} else if !errors.Is(err, os.ErrNotExist) {
			return result, err
		}
	}
	allowed := map[string]struct{}{filepath.Base(journalPath): {}}
	for _, artifact := range journal.Completed {
		allowed[filepath.Base(artifact.Path)] = struct{}{}
		info, err := os.Lstat(artifact.Path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return result, fmt.Errorf("rollback artifact is unsafe: %s", artifact.Path)
		}
	}
	entries, err := os.ReadDir(nodeDir)
	if err != nil {
		return result, err
	}
	for _, entry := range entries {
		if _, ok := allowed[entry.Name()]; !ok {
			return result, fmt.Errorf("rollback node directory contains unexpected entry %q", entry.Name())
		}
	}
	for index := len(journal.Completed) - 1; index >= 0; index-- {
		artifact := journal.Completed[index]
		if _, err := os.Lstat(artifact.Path); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if apply {
			inside, err := fsutil.IsWithin(nodeDir, artifact.Path)
			if err != nil || !inside {
				return result, fmt.Errorf("rollback artifact escaped node root: %s", artifact.Path)
			}
			if err := os.Remove(artifact.Path); err != nil {
				return result, err
			}
		}
		result.Actions = append(result.Actions, RollbackAction{Path: artifact.Path, Kind: artifact.Kind, Applied: apply})
	}
	if apply {
		if err := os.Remove(journalPath); err != nil {
			return result, err
		}
	}
	result.Actions = append(result.Actions, RollbackAction{Path: journalPath, Kind: "journal", Applied: apply})
	if apply {
		entries, err := os.ReadDir(nodeDir)
		if err != nil || len(entries) != 0 {
			return result, errors.New("rollback node directory changed before removal")
		}
		if err := os.Remove(nodeDir); err != nil {
			return result, err
		}
		if err := fsutil.SyncDir(filepath.Dir(nodeDir)); err != nil {
			return result, err
		}
	}
	result.Actions = append(result.Actions, RollbackAction{Path: nodeDir, Kind: "node-directory", Applied: apply})
	return result, nil
}
