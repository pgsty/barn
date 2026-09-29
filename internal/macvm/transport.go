package macvm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Guest connections go through Apple's own tools. macOS local network privacy
// stops third-party processes such as barn from connecting to directly
// attached subnets, including a machine's private network, unless the app
// responsible for them was authorized; the failure surfaces as "no route to
// host". Apple's platform tools are exempt, so the CLI reaches the guest
// through /usr/bin/nc here and /usr/bin/ssh for interactive sessions. The
// guest network is private to its runner: nothing else is reachable on it.
const (
	netcatPath = "/usr/bin/nc"
	SSHPath    = "/usr/bin/ssh"
)

// processConn is a net.Conn over a relay process's stdin and stdout.
type processConn struct {
	cmd     *exec.Cmd
	reader  *os.File
	writer  *os.File
	stderr  *bytes.Buffer
	address string
	done    chan struct{}
	once    sync.Once
	waitErr error
}

// dialGuest connects to address:port through /usr/bin/nc.
func dialGuest(ctx context.Context, address string, port int) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	childIn, parentOut, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	parentIn, childOut, err := os.Pipe()
	if err != nil {
		_ = childIn.Close()
		_ = parentOut.Close()
		return nil, err
	}
	timeout := 8
	if deadline, ok := ctx.Deadline(); ok {
		if seconds := int(time.Until(deadline).Seconds()); seconds < timeout {
			timeout = max(1, seconds)
		}
	}
	stderr := &bytes.Buffer{}
	cmd := exec.Command(netcatPath, "-v", "-G", strconv.Itoa(timeout), address, strconv.Itoa(port))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = childIn, childOut, &limitedWriter{buffer: stderr, limit: 4096}
	err = cmd.Start()
	_ = childIn.Close()
	_ = childOut.Close()
	if err != nil {
		_ = parentIn.Close()
		_ = parentOut.Close()
		return nil, fmt.Errorf("start %s: %w", netcatPath, err)
	}
	conn := &processConn{cmd: cmd, reader: parentIn, writer: parentOut, stderr: stderr, address: net.JoinHostPort(address, strconv.Itoa(port)), done: make(chan struct{})}
	go func() {
		conn.waitErr = cmd.Wait()
		close(conn.done)
	}()
	return conn, nil
}

func (c *processConn) Read(p []byte) (int, error)  { return c.reader.Read(p) }
func (c *processConn) Write(p []byte) (int, error) { return c.writer.Write(p) }

func (c *processConn) Close() error {
	c.once.Do(func() {
		_ = c.writer.Close()
		_ = c.reader.Close()
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
	})
	<-c.done
	return nil
}

// failure explains a connection that ended before SSH could start, using the
// relay's own message such as "Connection refused".
func (c *processConn) failure(cause error) error {
	select {
	case <-c.done:
	case <-time.After(300 * time.Millisecond):
		return cause
	}
	message := strings.TrimSpace(c.stderr.String())
	i := strings.LastIndex(message, "failed: ")
	if i < 0 {
		return cause
	}
	return fmt.Errorf("connect to %s: %s", c.address, message[i+len("failed: "):])
}

func (c *processConn) LocalAddr() net.Addr  { return relayAddr("barn") }
func (c *processConn) RemoteAddr() net.Addr { return relayAddr(c.address) }
func (c *processConn) SetDeadline(t time.Time) error {
	return errors.Join(c.reader.SetReadDeadline(t), c.writer.SetWriteDeadline(t))
}
func (c *processConn) SetReadDeadline(t time.Time) error  { return c.reader.SetReadDeadline(t) }
func (c *processConn) SetWriteDeadline(t time.Time) error { return c.writer.SetWriteDeadline(t) }

type relayAddr string

func (a relayAddr) Network() string { return "tcp" }
func (a relayAddr) String() string  { return string(a) }

type limitedWriter struct {
	mu     sync.Mutex
	buffer *bytes.Buffer
	limit  int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if room := w.limit - w.buffer.Len(); room > 0 {
		_, _ = w.buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

var _ io.ReadWriteCloser = (*processConn)(nil)
