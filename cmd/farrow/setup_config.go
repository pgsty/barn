package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pgsty/farrow/internal/config"
	"github.com/pgsty/farrow/internal/fsutil"
	"github.com/pgsty/farrow/internal/state"
)

// Only an untouched built-in default in a fresh deployment is eligible. An
// explicit -f, custom inventory, or any existing node keeps its chosen network.
func recognizeDefaultSetupTemplate(selection setupSelection) setupSelection {
	root, err := state.ResolveDataRoot()
	if err != nil {
		return selection
	}
	if _, err := (state.Store{Root: root}).ReadDeployment(); !errors.Is(err, os.ErrNotExist) {
		return selection
	}
	if nodes, err := os.ReadDir(filepath.Join(root, "nodes")); err != nil && !errors.Is(err, os.ErrNotExist) || len(nodes) != 0 {
		return selection
	}
	data, err := os.ReadFile(selection.ConfigPath)
	if err != nil {
		return selection
	}
	for _, profile := range config.TemplateNames() {
		template, err := config.Template(profile, "")
		if err == nil && bytes.Equal(template, data) {
			selection.Profile, selection.Generated = profile, true
			selection.OriginalConfig = data
			return selection
		}
	}
	return selection
}

func publishSetupConfig(selection setupSelection) error {
	if len(selection.OriginalConfig) == 0 {
		return fsutil.AtomicCreate(selection.ConfigPath, selection.ConfigData, 0600)
	}
	info, err := os.Lstat(selection.ConfigPath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("generated inventory is no longer a regular file")
	}
	current, err := os.ReadFile(selection.ConfigPath)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, selection.OriginalConfig) {
		return fmt.Errorf("%s changed during setup; its content was preserved", selection.ConfigPath)
	}
	backup := selection.ConfigPath + ".before-network-change"
	// An earlier rebase of this same default template already kept it; any
	// other existing backup is never overwritten.
	if !sameRegularFile(backup, current) {
		if err := fsutil.AtomicCreate(backup, current, info.Mode().Perm()); err != nil {
			return fmt.Errorf("preserve original inventory at %s: %w", backup, err)
		}
	}
	current, err = os.ReadFile(selection.ConfigPath)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, selection.OriginalConfig) {
		return fmt.Errorf("%s changed during setup; its content was preserved", selection.ConfigPath)
	}
	return fsutil.AtomicWrite(selection.ConfigPath, selection.ConfigData, info.Mode().Perm())
}

// sameRegularFile reports whether path is a regular file holding exactly data.
func sameRegularFile(path string, data []byte) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != int64(len(data)) {
		return false
	}
	current, err := os.ReadFile(path)
	return err == nil && bytes.Equal(current, data)
}
