package darwin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/execx"
)

type repairRunner struct {
	calls  []string
	absent bool
	fail   string
}

func (r *repairRunner) Run(_ context.Context, binary string, args ...string) (execx.Result, error) {
	call := strings.Join(append([]string{binary}, args...), " ")
	r.calls = append(r.calls, call)
	if r.absent && args[0] == "print" || args[0] == r.fail {
		return execx.Result{}, errors.New("fixture launchctl failure")
	}
	return execx.Result{}, nil
}

func TestRepairRestartsOnlyTheRecordedBarnService(t *testing.T) {
	for _, absent := range []bool{false, true} {
		runner := &repairRunner{absent: absent}
		if err := (Executor{Root: runner}).restartService(context.Background()); err != nil {
			t.Fatal(err)
		}
		got := strings.Join(runner.calls, "\n")
		if strings.Contains(got, "bootstrap") != absent || !strings.HasSuffix(got, "/bin/launchctl kickstart -k system/"+ServiceID) || strings.Contains(got, "bootout") {
			t.Fatalf("unexpected repair commands: %s", got)
		}
	}
	runner := &repairRunner{absent: true, fail: "bootstrap"}
	if err := (Executor{Root: runner}).restartService(context.Background()); err == nil || len(runner.calls) != 2 {
		t.Fatalf("repair continued after failure: %v %v", runner.calls, err)
	}
}
