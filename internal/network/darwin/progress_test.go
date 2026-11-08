package darwin

import (
	"context"
	"testing"

	"github.com/pgsty/barn/internal/activity"
)

func TestInstallProgressMarksPlanFailureBeforeMutation(t *testing.T) {
	var events []activity.Event
	executor := Executor{Progress: func(event activity.Event) { events = append(events, event) }}
	report, err := executor.InstallModeNetwork(context.Background(), "", "", "arm64", "host", "10.10.10.0/24", true)
	if err == nil || report.Applied || len(events) != 2 {
		t.Fatalf("report=%+v err=%v events=%+v", report, err, events)
	}
	last := events[len(events)-1]
	if last.Item != "plan" || last.State != "failed" || !last.Done || last.Message != err.Error() {
		t.Fatalf("lost plan failure: %+v", last)
	}
}
