package provision

import (
	"bytes"
	"io"
	"strings"
	"time"
)

// Keep the newest line for presentation while the original writer retains the
// bounded command output. A script with no line breaks must also stay bounded.
type progressOutput struct {
	output  io.Writer
	publish func(string)
	pending []byte
	latest  string
	last    time.Time
}

func (out *progressOutput) Write(data []byte) (int, error) {
	n, err := out.output.Write(data)
	remaining := data[:n]
	for len(remaining) > 0 {
		end := bytes.IndexAny(remaining, "\r\n")
		part := remaining
		if end >= 0 {
			part = remaining[:end]
		}
		const limit = 4096
		if len(part) >= limit {
			out.pending = append(out.pending[:0], part[len(part)-limit:]...)
		} else {
			if excess := len(out.pending) + len(part) - limit; excess > 0 {
				out.pending = out.pending[excess:]
			}
			out.pending = append(out.pending, part...)
		}
		if end < 0 {
			break
		}
		if line := strings.TrimSpace(string(out.pending)); line != "" {
			out.latest = line
		}
		out.pending = out.pending[:0]
		remaining = remaining[end+1:]
	}
	if time.Since(out.last) >= 100*time.Millisecond {
		out.flush()
	}
	return n, err
}

func (out *progressOutput) flush() {
	line := out.latest
	if pending := strings.TrimSpace(string(out.pending)); pending != "" {
		line = pending
	}
	if line != "" {
		out.publish(line)
		out.last = time.Now()
		out.latest = ""
	}
}
