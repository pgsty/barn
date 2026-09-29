package macvm

import (
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/activity"
)

func TestRunnerProgressBecomesEvents(t *testing.T) {
	var events []activity.Event
	writer := NewProgressWriter(func(event activity.Event) { events = append(events, event) })
	lines := `{"phase":"validating_image","time":"x"}
{"phase":"restoring","fraction":0.42}
{"phase":"network","mode":"vmnet","address":"10.10.20.10"}
not json
{"phase":"restored","seconds":1}
`
	// Split writes must still produce whole lines.
	for _, chunk := range []string{lines[:17], lines[17:60], lines[60:]} {
		if _, err := writer.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if len(events) != 5 {
		t.Fatalf("events %+v", events)
	}
	if events[0].Phase != "image-verify" || !strings.Contains(events[1].Message, "42%") || events[2].Phase != "network" || !strings.Contains(events[2].Message, "10.10.20.10") {
		t.Fatalf("events %+v", events)
	}
	if events[3].Message != "not json" || !events[4].Done {
		t.Fatalf("events %+v", events)
	}
}
