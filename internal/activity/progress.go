package activity

import (
	"io"
	"time"
)

const defaultProgressInterval = 250 * time.Millisecond

// ProgressReader wraps an io.Reader and reports throttled byte progress events.
type ProgressReader struct {
	reader   io.Reader
	reporter Reporter
	event    Event
	interval time.Duration
	current  int64
	last     time.Time
}

// NewProgressReader returns a reader that reports byte counts against event.
// event.CurrentBytes seeds the starting offset for resumed transfers.
func NewProgressReader(reader io.Reader, reporter Reporter, event Event, interval time.Duration) *ProgressReader {
	if interval <= 0 {
		interval = defaultProgressInterval
	}
	return &ProgressReader{
		reader:   reader,
		reporter: reporter,
		event:    event,
		interval: interval,
		current:  event.CurrentBytes,
		last:     time.Now(),
	}
}

func (r *ProgressReader) Read(buffer []byte) (int, error) {
	read, err := r.reader.Read(buffer)
	r.current += int64(read)
	now := time.Now()
	if (read > 0 && (r.last.IsZero() || now.Sub(r.last) >= r.interval)) || err == io.EOF {
		r.event.CurrentBytes = r.current
		r.reporter.Report(r.event)
		r.last = now
	}
	return read, err
}

// Bytes returns the total number of bytes observed so far (including the initial offset).
func (r *ProgressReader) Bytes() int64 { return r.current }

// CopyWithProgress copies up to limit bytes from source to destination while
// emitting start, periodic, and completion progress events.
func CopyWithProgress(destination io.Writer, source io.Reader, limit int64, reporter Reporter, event Event) (int64, error) {
	event.StartBytes = event.CurrentBytes
	if event.StartedAt.IsZero() {
		event.StartedAt = time.Now()
	}
	reporter.Report(event)
	tracked := NewProgressReader(io.LimitReader(source, limit), reporter, event, defaultProgressInterval)
	written, err := io.Copy(destination, tracked)
	event.CurrentBytes = tracked.Bytes()
	event.Done = err == nil
	reporter.Report(event)
	return written, err
}
