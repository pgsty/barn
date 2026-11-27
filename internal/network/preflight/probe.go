package preflight

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/pgsty/barn/internal/execx"
)

type DialFunc func(network, address string, timeout time.Duration) (net.Conn, error)

type Probe struct {
	Runner   execx.Runner
	Lstat    func(string) (os.FileInfo, error)
	ReadFile func(string) ([]byte, error)
	Dial     DialFunc
}

func (p Probe) lstat(path string) (os.FileInfo, error) {
	if p.Lstat != nil {
		return p.Lstat(path)
	}
	return os.Lstat(path)
}

func (p Probe) readFile(path string) ([]byte, error) {
	if p.ReadFile != nil {
		return p.ReadFile(path)
	}
	return os.ReadFile(path)
}

func (p Probe) dial(network, address string, timeout time.Duration) (net.Conn, error) {
	if p.Dial != nil {
		return p.Dial(network, address, timeout)
	}
	return net.DialTimeout(network, address, timeout)
}

func (p Probe) Collect(ctx context.Context, request Request) Snapshot {
	snapshot := Snapshot{Addresses: make(map[string]string)}
	if p.Runner == nil {
		snapshot.Problems = append(snapshot.Problems, "network preflight requires a command runner")
		return snapshot
	}
	switch request.OS {
	case "darwin":
		snapshot = p.collectDarwin(ctx, request)
	case "linux":
		snapshot = p.collectLinux(ctx, request)
	default:
		snapshot.Problems = append(snapshot.Problems, "network preflight does not support "+request.OS)
	}
	if snapshot.Addresses == nil {
		snapshot.Addresses = make(map[string]string)
	}
	var addressMu sync.Mutex
	var addressWG sync.WaitGroup
	addressSlots := make(chan struct{}, 32)
	for _, address := range request.Addresses {
		addressWG.Add(1)
		go func() {
			defer addressWG.Done()
			select {
			case addressSlots <- struct{}{}:
				defer func() { <-addressSlots }()
			case <-ctx.Done():
				return
			}
			connection, err := p.dial("tcp", net.JoinHostPort(address, "22"), 250*time.Millisecond)
			if err != nil {
				return
			}
			_ = connection.Close()
			addressMu.Lock()
			snapshot.Addresses[address] = address + " already answers on port 22"
			addressMu.Unlock()
		}()
	}
	addressWG.Wait()
	return snapshot
}

func Run(ctx context.Context, request Request, probe Probe) Report {
	return Evaluate(request, probe.Collect(ctx, request))
}

func rootMetadata(info os.FileInfo, mode os.FileMode, kind string, group uint32, sticky bool) error {
	statistics, ok := info.Sys().(*syscall.Stat_t)
	if !ok || statistics.Uid != 0 || statistics.Gid != group || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != mode || (sticky && info.Mode()&os.ModeSticky == 0) {
		stickyText := ""
		if sticky {
			stickyText = " sticky"
		}
		return fmt.Errorf("not a root-owned group-%d non-symlink mode-%04o%s %s", group, mode, stickyText, kind)
	}
	switch kind {
	case "file":
		if !info.Mode().IsRegular() {
			return errors.New("not a regular file")
		}
	case "directory":
		if !info.IsDir() {
			return errors.New("not a directory")
		}
	case "socket":
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("not a Unix socket")
		}
	}
	return nil
}

func invalidate(installation *Installation, problem string) {
	installation.Status = "invalid"
	if installation.Problem == "" {
		installation.Problem = problem
	}
}

func (p Probe) exactPublicFile(path string, expected []byte, max int) error {
	data, err := p.readFile(path)
	if err != nil {
		return err
	}
	if len(data) > max || !bytes.Equal(data, expected) {
		return errors.New("content differs from the pinned plan")
	}
	return nil
}

func protectedInspectionError(err error) bool {
	return errors.Is(err, os.ErrPermission)
}
