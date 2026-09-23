package process

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func processStarted(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", fmt.Errorf("read Linux process start identity: %w", err)
	}
	return parseProcStatStart(data)
}

func parseProcStatStart(data []byte) (string, error) {
	line := strings.TrimSpace(string(data))
	closing := strings.LastIndexByte(line, ')')
	if closing < 0 || closing+1 >= len(line) {
		return "", errors.New("process stat lacks a complete command field")
	}
	fields := strings.Fields(line[closing+1:])
	// fields[0] is stat field 3 (state); field 22 (starttime) is index 19.
	if len(fields) <= 19 {
		return "", errors.New("process stat lacks starttime field 22")
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || start == 0 {
		return "", errors.New("process stat starttime is invalid")
	}
	return "procstat:" + strconv.FormatUint(start, 10), nil
}

func processArgv(pid int) ([]string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil, fmt.Errorf("read Linux process argv: %w", err)
	}
	return parseNULArgv(data)
}

// BootID identifies the current host boot; it changes on every restart.
func BootID() (string, error) {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", fmt.Errorf("read Linux boot id: %w", err)
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", errors.New("linux boot id is empty")
	}
	return value, nil
}

func parseNULArgv(data []byte) ([]string, error) {
	if len(data) == 0 {
		return nil, errors.New("process argv bytes are empty")
	}
	if data[len(data)-1] == 0 {
		data = data[:len(data)-1]
	}
	parts := bytes.Split(data, []byte{0})
	argv := make([]string, len(parts))
	for index, part := range parts {
		argv[index] = string(part)
	}
	if len(argv) == 0 || argv[0] == "" {
		return nil, errors.New("process argv has no executable")
	}
	return argv, nil
}
