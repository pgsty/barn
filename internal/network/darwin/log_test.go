package darwin

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/execx"
)

type logFileInfo struct {
	mode     os.FileMode
	uid, gid uint32
}

func (f logFileInfo) Name() string       { return "barn-vmnet" }
func (f logFileInfo) Size() int64        { return 0 }
func (f logFileInfo) Mode() os.FileMode  { return f.mode }
func (f logFileInfo) ModTime() time.Time { return time.Time{} }
func (f logFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f logFileInfo) Sys() any           { return &syscall.Stat_t{Uid: f.uid, Gid: f.gid} }

func TestRestoreLogDirectoryOnlyChangesSafeOwnedDirectory(t *testing.T) {
	for _, test := range []struct {
		name    string
		info    os.FileInfo
		command string
		unsafe  bool
	}{
		{name: "launchd recreated directory", info: logFileInfo{mode: os.ModeDir | 0o744}, command: "/bin/chmod\x000755\x00" + LogDir},
		{name: "missing directory", command: "/usr/bin/install\x00-d\x00-o\x00root\x00-g\x00wheel\x00-m\x000755\x00" + LogDir},
		{name: "already correct", info: logFileInfo{mode: os.ModeDir | 0o755}},
		{name: "symlink", info: logFileInfo{mode: os.ModeSymlink | 0o755}, unsafe: true},
		{name: "regular file", info: logFileInfo{mode: 0o744}, unsafe: true},
		{name: "user owned", info: logFileInfo{mode: os.ModeDir | 0o744, uid: 501}, unsafe: true},
		{name: "wrong group", info: logFileInfo{mode: os.ModeDir | 0o744, gid: 20}, unsafe: true},
		{name: "group writable", info: logFileInfo{mode: os.ModeDir | 0o775}, unsafe: true},
		{name: "world writable", info: logFileInfo{mode: os.ModeDir | 0o777}, unsafe: true},
		{name: "special mode", info: logFileInfo{mode: os.ModeDir | os.ModeSetgid | 0o755}, unsafe: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &recordingRunner{}
			changed, err := restoreLogDirectory(context.Background(), runner, test.info)
			if (err != nil) != test.unsafe || changed != (test.command != "") {
				t.Fatalf("changed=%t err=%v", changed, err)
			}
			if got := strings.Join(runner.calls, "\n"); got != test.command {
				t.Fatalf("commands=%q, want %q", got, test.command)
			}
		})
	}
}

type deniedLogRepair struct{}

func (deniedLogRepair) Run(context.Context, string, ...string) (execx.Result, error) {
	return execx.Result{}, errors.New("sudo: a password is required")
}

func TestLogDirectoryRepairReportsSudoFailure(t *testing.T) {
	changed, err := restoreLogDirectory(context.Background(), deniedLogRepair{}, logFileInfo{mode: os.ModeDir | 0o744})
	if changed || err == nil || !strings.Contains(err.Error(), LogDir) || !strings.Contains(err.Error(), "password") {
		t.Fatalf("changed=%t err=%v", changed, err)
	}
}
