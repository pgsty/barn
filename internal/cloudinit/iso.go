package cloudinit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
)

const maxSeedBytes = 4 << 20

func (f Files) entries() map[string][]byte {
	return map[string][]byte{
		"meta-data":      f.MetaData,
		"user-data":      f.UserData,
		"network-config": f.NetworkConfig,
	}
}

func validateFiles(files Files) error {
	total := 0
	for name, content := range files.entries() {
		if len(content) == 0 {
			return fmt.Errorf("CIDATA file %s is empty", name)
		}
		total += len(content)
	}
	if total > maxSeedBytes {
		return fmt.Errorf("CIDATA payload %d exceeds %d bytes", total, maxSeedBytes)
	}
	return nil
}

// BuildISO atomically publishes a mode-0600 ISO9660 image with exactly the
// three NoCloud root files and a CIDATA volume label.
func BuildISO(target string, files Files) error {
	if err := validateFiles(files); err != nil {
		return err
	}
	if target == "" || !filepath.IsAbs(target) {
		return errors.New("CIDATA target must be an absolute path")
	}
	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("CIDATA target already exists: %s", target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat CIDATA target: %w", err)
	}
	parent := filepath.Dir(target)
	parentInfo, err := os.Stat(parent)
	if err != nil || !parentInfo.IsDir() {
		return fmt.Errorf("CIDATA parent is not an existing directory: %s", parent)
	}
	temp, err := os.CreateTemp(parent, "."+filepath.Base(target)+".partial-")
	if err != nil {
		return fmt.Errorf("create CIDATA temporary file: %w", err)
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}()
	if err := temp.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod CIDATA temporary file: %w", err)
	}

	backend := file.New(temp, false)
	fs, err := iso9660.Create(backend, 0, 0, 2048, "")
	if err != nil {
		return fmt.Errorf("create ISO9660 filesystem: %w", err)
	}
	for name, content := range files.entries() {
		isoFile, openErr := fs.OpenFile("/"+name, os.O_CREATE|os.O_RDWR)
		if openErr != nil {
			return fmt.Errorf("create CIDATA file %s: %w", name, openErr)
		}
		if _, writeErr := isoFile.Write(content); writeErr != nil {
			_ = isoFile.Close()
			return fmt.Errorf("write CIDATA file %s: %w", name, writeErr)
		}
		if closeErr := isoFile.Close(); closeErr != nil {
			return fmt.Errorf("close CIDATA file %s: %w", name, closeErr)
		}
	}
	if err := fs.Finalize(iso9660.FinalizeOptions{RockRidge: true, VolumeIdentifier: "CIDATA", PublisherIdentifier: "Barn"}); err != nil {
		return fmt.Errorf("finalize CIDATA ISO9660: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync CIDATA image: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close CIDATA image: %w", err)
	}
	if err := os.Rename(tempPath, target); err != nil {
		return fmt.Errorf("publish CIDATA image: %w", err)
	}
	directory, err := os.Open(parent)
	if err != nil {
		return fmt.Errorf("open CIDATA parent for sync: %w", err)
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return fmt.Errorf("sync CIDATA parent: %w", err)
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close CIDATA parent: %w", err)
	}
	return nil
}
