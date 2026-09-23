package process

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func processStarted(pid int) (string, error) {
	value, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", fmt.Errorf("read Darwin process start identity: %w", err)
	}
	started := value.Proc.P_starttime
	if started.Sec <= 0 || started.Usec < 0 || started.Usec >= 1_000_000 {
		return "", errors.New("darwin process start identity is invalid")
	}
	return fmt.Sprintf("kinfo:%d.%06d", started.Sec, started.Usec), nil
}

func processArgv(pid int) ([]string, error) {
	data, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, fmt.Errorf("read darwin process argv: %w", err)
	}
	return parseDarwinProcArgs(data)
}
