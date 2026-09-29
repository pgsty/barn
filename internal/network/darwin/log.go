package darwin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/pgsty/barn/internal/execx"
)

// LogDirectoryNeedsRepair accepts a missing directory or a root-owned directory
// whose permissions can safely be restored. In particular, launchd may recreate
// a missing log parent with mode 0744. Never adopt symlinks or writable targets.
func LogDirectoryNeedsRepair(info os.FileInfo) (bool, error) {
	if info == nil {
		return true, nil
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, fmt.Errorf("%s: cannot verify directory ownership", LogDir)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || stat.Uid != 0 || stat.Gid != 0 ||
		info.Mode().Perm()&0o022 != 0 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return false, fmt.Errorf("%s: got %s uid=%d gid=%d mode=%04o; expected a non-symlink root:wheel directory with mode 0755; Barn will not repair it automatically", LogDir, info.Mode(), stat.Uid, stat.Gid, info.Mode().Perm())
	}
	return info.Mode().Perm() != 0o755, nil
}

func restoreLogDirectory(ctx context.Context, runner execx.Runner, info os.FileInfo) (bool, error) {
	needsRepair, err := LogDirectoryNeedsRepair(info)
	if err != nil || !needsRepair {
		return false, err
	}
	if info == nil {
		_, err = runner.Run(ctx, "/usr/bin/install", "-d", "-o", "root", "-g", "wheel", "-m", "0755", LogDir)
	} else {
		_, err = runner.Run(ctx, "/bin/chmod", "0755", LogDir)
	}
	if err != nil {
		return false, fmt.Errorf("restore %s permissions: %w", LogDir, err)
	}
	return true, nil
}

// Called only after readInstalled has verified the root-owned installation.
func (e Executor) repairLogDirectory(ctx context.Context) (bool, error) {
	info, err := os.Lstat(LogDir)
	if errors.Is(err, os.ErrNotExist) {
		info, err = nil, nil
	}
	if err != nil {
		return false, err
	}
	changed, err := restoreLogDirectory(ctx, e.Root, info)
	if err != nil {
		return false, err
	}
	return changed, e.rootStat(ctx, LogDir, "root", "wheel", "755", "Directory")
}
