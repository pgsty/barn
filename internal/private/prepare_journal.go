package private

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pgsty/barn/internal/identity"
	"github.com/pgsty/barn/internal/naming"
)

const maxPrepareJournalBytes = 1 << 20

func validatePrepareJournal(path string, value PrepareJournal) error {
	if value.Schema != 1 || !identity.ValidUUID(value.OperationID) || !identity.ValidUUID(value.VMUUID) || !naming.ValidNodeName(value.Node) || len(value.SpecHash) != 64 || value.StartedAt.IsZero() || value.UpdatedAt.Before(value.StartedAt) {
		return errors.New("prepare journal identity, hash, or time is invalid")
	}
	if _, err := hex.DecodeString(value.SpecHash); err != nil {
		return err
	}
	if filepath.Base(path) != "private-prepare.json" {
		return errors.New("prepare journal filename is invalid")
	}
	nodeDir := filepath.Dir(path)
	seen := make(map[string]struct{})
	for _, artifact := range value.Completed {
		if artifact.Path == "" || !filepath.IsAbs(artifact.Path) || filepath.Dir(artifact.Path) != nodeDir {
			return errors.New("prepare artifact escapes the node directory")
		}
		basename := filepath.Base(artifact.Path)
		dataName := strings.TrimSuffix(basename, ".qcow2")
		valid := (artifact.Kind == "root-overlay" && basename == "root.qcow2") ||
			(artifact.Kind == "seed" && basename == "seed.iso") ||
			(artifact.Kind == "nvram" && basename == "nvram.fd") ||
			(artifact.Kind == "data-disk" && strings.HasSuffix(basename, ".qcow2") && dataName != "root" && naming.ValidNodeName(dataName))
		if !valid {
			return errors.New("prepare artifact kind/path is outside the allowlist")
		}
		if _, duplicate := seen[artifact.Path]; duplicate {
			return errors.New("prepare journal repeats an artifact")
		}
		seen[artifact.Path] = struct{}{}
	}
	if value.Prepared && (value.Invocation.Binary == "" || !filepath.IsAbs(value.Invocation.Binary) || len(value.Invocation.Args) == 0) {
		return errors.New("prepared private journal lacks a typed invocation")
	}
	if !value.Prepared && (value.Invocation.Binary != "" || len(value.Invocation.Args) != 0) {
		return errors.New("partial private journal unexpectedly contains an invocation")
	}
	if value.StateCommitted {
		if !value.Prepared || value.StatePath != filepath.Join(nodeDir, "state.json") {
			return errors.New("committed private journal lacks the exact node state path")
		}
	} else if value.StatePath != "" {
		return errors.New("uncommitted private journal unexpectedly names state")
	}
	return nil
}

func ReadPrepareJournal(path string) (PrepareJournal, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return PrepareJournal{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 || info.Size() > maxPrepareJournalBytes {
		return PrepareJournal{}, errors.New("prepare journal is unsafe")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return PrepareJournal{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var value PrepareJournal
	if err := decoder.Decode(&value); err != nil {
		return PrepareJournal{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return PrepareJournal{}, errors.New("prepare journal has trailing JSON data")
	}
	if err := validatePrepareJournal(path, value); err != nil {
		return PrepareJournal{}, err
	}
	return value, nil
}
