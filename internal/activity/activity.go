// Package activity carries best-effort, human-facing progress updates from
// long-running operations to the CLI without coupling core packages to a
// terminal renderer.
package activity

import "time"

// Event describes the operation currently in progress. Byte fields are used
// for transfers and verification passes; zero values keep ordinary stage
// updates compact.
type Event struct {
	CPUs         int
	Memory       int64
	Phase        string
	Message      string
	Source       string
	CurrentBytes int64
	StartBytes   int64
	TotalBytes   int64
	StartedAt    time.Time
	Done         bool
	Node         string
	Item         string
	Address      string
	State        string
}

// Reporter receives best-effort presentation updates. Reporters must not
// affect the success or failure of the underlying operation. Parallel node
// operations may call a reporter concurrently.
type Reporter func(Event)

func (reporter Reporter) Report(event Event) {
	if reporter != nil {
		reporter(event)
	}
}
