package linuxvm

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
)

func readSeedISO(path string) (string, map[string][]byte, error) {
	handle, err := os.Open(path)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = handle.Close() }()
	info, err := handle.Stat()
	if err != nil {
		return "", nil, err
	}
	fs, err := iso9660.Read(file.New(handle, true), info.Size(), 0, 2048)
	if err != nil {
		return "", nil, err
	}
	contents := make(map[string][]byte, 3)
	for _, name := range []string{"meta-data", "user-data", "network-config"} {
		isoFile, openErr := fs.OpenFile("/"+name, os.O_RDONLY)
		if openErr != nil {
			return "", nil, fmt.Errorf("open %s from CIDATA: %w", name, openErr)
		}
		content, readErr := io.ReadAll(isoFile)
		closeErr := isoFile.Close()
		if readErr != nil {
			return "", nil, fmt.Errorf("read %s from CIDATA: %w", name, readErr)
		}
		if closeErr != nil {
			return "", nil, fmt.Errorf("close %s from CIDATA: %w", name, closeErr)
		}
		contents[name] = content
	}
	return strings.TrimRight(fs.Label(), " \x00"), contents, nil
}
