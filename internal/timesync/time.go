// Package timesync compares guest wall clocks with the host over management SSH.
package timesync

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"time"

	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/vm"
)

const (
	Threshold    = 2 * time.Second
	MaxRoundTrip = 250 * time.Millisecond
	Timeout      = 15 * time.Second
)

//go:embed guest.py
var guestScript string

type NTP struct {
	Synchronized *bool  `json:"synchronized"`
	Service      string `json:"service"`
	Source       string `json:"source,omitempty"`
}

type Measurement struct {
	OffsetMS      float64   `json:"offset_ms"`
	UncertaintyMS float64   `json:"uncertainty_ms"`
	RoundTripMS   float64   `json:"round_trip_ms"`
	HostTime      time.Time `json:"host_time"`
	GuestTime     time.Time `json:"guest_time"`
	MonotonicNS   int64     `json:"-"`
}

type Result struct {
	Action string       `json:"action"`
	Before *Measurement `json:"before,omitempty"`
	After  *Measurement `json:"after,omitempty"`
	NTP    NTP          `json:"ntp"`
}

type Options struct {
	Sync             bool
	Automatic        bool
	SkipBusy         bool
	RecentlyAdjusted bool
	LastAdjustedAt   time.Time
	LastMonotonicNS  int64
	LatchedConflict  bool
}

type message struct {
	Ready       bool   `json:"ready"`
	Adjusted    bool   `json:"adjusted"`
	UnixNS      int64  `json:"unix_ns"`
	MonotonicNS int64  `json:"monotonic_ns"`
	NTP         NTP    `json:"ntp"`
	Error       string `json:"error"`
}

// Run authenticates once and samples only after the remote process is ready.
// The caller checks the QEMU identity and supplies instance-scoped SSH args.
func Run(parent context.Context, runner execx.Runner, sshPath string, args []string, expected vm.ReadyMarker, options Options) (Result, error) {
	duplex, ok := runner.(execx.DuplexRunner)
	if !ok {
		return Result{}, errors.New("SSH runner cannot exchange clock samples")
	}
	identity, err := json.Marshal(expected)
	if err != nil {
		return Result{}, err
	}
	args = append(append([]string(nil), args...), vm.QuoteRemote([]string{"sudo", "-n", "python3", "-u", "-c", guestScript, string(identity)}))
	ctx, cancel := context.WithTimeout(parent, Timeout)
	defer cancel()
	// An OS pipe is inherited directly by ssh. An io.Pipe would make os/exec
	// wait for a blocked stdin-copy goroutine when SSH exits before reading.
	input, requests, err := os.Pipe()
	if err != nil {
		return Result{}, fmt.Errorf("open clock request pipe: %w", err)
	}
	responses, output := io.Pipe()
	finished := make(chan error, 1)
	go func() {
		_, runErr := duplex.RunWithIO(ctx, sshPath, input, output, args...)
		_ = output.CloseWithError(runErr)
		_ = input.Close()
		finished <- runErr
	}()
	stopClose := context.AfterFunc(ctx, func() {
		_ = responses.CloseWithError(ctx.Err())
		_ = requests.Close()
	})
	result, sessionErr := session(responses, requests, options)
	_ = requests.Close()
	if sessionErr != nil {
		cancel()
	} else {
		// A JSON decoder can return before the helper's trailing newline reaches
		// stdout. Drain the closed session before closing the reader, otherwise
		// os/exec can turn a successful check into an io.ErrClosedPipe failure.
		_, sessionErr = io.Copy(io.Discard, io.LimitReader(responses, 64<<10))
	}
	_ = responses.Close()
	runErr := <-finished
	stopClose()
	if sessionErr != nil || runErr != nil {
		return result, fmt.Errorf("synchronize guest clock: %w", errors.Join(sessionErr, runErr))
	}
	return result, nil
}

func receive(decoder *json.Decoder) (message, error) {
	var value message
	if err := decoder.Decode(&value); err != nil {
		return value, fmt.Errorf("read clock response: %w", err)
	}
	if value.Error != "" {
		return value, fmt.Errorf("guest clock: %s", value.Error)
	}
	return value, nil
}

func session(reader io.Reader, writer io.Writer, options Options) (Result, error) {
	decoder := json.NewDecoder(io.LimitReader(reader, 64<<10))
	encoder := json.NewEncoder(writer)
	hello, err := receive(decoder)
	result := Result{Action: "checked", NTP: hello.NTP}
	if err != nil {
		return result, err
	}
	if !hello.Ready {
		return result, errors.New("guest clock helper did not confirm identity")
	}
	before, err := measure(encoder, decoder)
	if err != nil {
		return result, err
	}
	result.Before = &before
	if !options.Sync {
		return result, nil
	}
	if options.Automatic && math.Abs(before.OffsetMS) <= float64(Threshold/time.Millisecond) {
		result.Action = "within-threshold"
		return result, nil
	}
	if hasSourceConflict(before, hello.NTP, options) {
		result.Action = "source-conflict"
		return result, errors.New("clock drift returned after a recent correction while guest NTP is synchronized; align the NTP source with the host or run barn time sync explicitly")
	}
	if err := encoder.Encode(map[string]any{"op": "set", "unix_ns": before.HostTime.UnixNano(), "monotonic_ns": before.MonotonicNS}); err != nil {
		return result, err
	}
	ack, err := receive(decoder)
	if err != nil {
		return result, err
	}
	if !ack.Adjusted {
		return result, errors.New("guest did not confirm clock adjustment")
	}
	result.Action = "adjusted"
	after, err := measure(encoder, decoder)
	if err != nil {
		return result, err
	}
	result.After = &after
	if math.Abs(after.OffsetMS) > 500 {
		return result, errors.New("guest clock remains more than 500ms from the host after adjustment")
	}
	return result, nil
}

func measure(encoder *json.Encoder, decoder *json.Decoder) (Measurement, error) {
	var best Measurement
	best.RoundTripMS = math.Inf(1)
	for range 3 {
		start := time.Now()
		if err := encoder.Encode(map[string]string{"op": "sample"}); err != nil {
			return best, err
		}
		value, err := receive(decoder)
		end := time.Now()
		if err != nil {
			return best, err
		}
		elapsed := end.Sub(start)
		wallElapsed := end.UnixNano() - start.UnixNano()
		if math.Abs(float64(wallElapsed-int64(elapsed))) > float64(100*time.Millisecond) {
			return best, errors.New("host clock changed during measurement; retry")
		}
		if value.UnixNS <= 0 || value.MonotonicNS <= 0 {
			return best, errors.New("guest returned an invalid clock sample")
		}
		if elapsed > MaxRoundTrip {
			continue
		}
		if float64(elapsed)/float64(time.Millisecond) < best.RoundTripMS {
			middle := start.Add(elapsed / 2)
			best = Measurement{OffsetMS: float64(value.UnixNS-middle.UnixNano()) / 1e6, RoundTripMS: float64(elapsed) / 1e6, UncertaintyMS: float64(elapsed) / 2e6, HostTime: middle.UTC(), GuestTime: time.Unix(0, value.UnixNS).UTC(), MonotonicNS: value.MonotonicNS}
		}
	}
	if math.IsInf(best.RoundTripMS, 1) {
		return Measurement{}, fmt.Errorf("clock measurement is unreliable: all round trips exceeded %s", MaxRoundTrip)
	}
	return best, nil
}

func hasSourceConflict(before Measurement, ntp NTP, options Options) bool {
	if !options.Automatic || ntp.Synchronized == nil || !*ntp.Synchronized {
		return false
	}
	// A guest reboot resets CLOCK_MONOTONIC, invalidating prior adjustment anchors.
	if options.LastMonotonicNS > 0 && before.MonotonicNS < options.LastMonotonicNS {
		return false
	}
	if options.LatchedConflict {
		return true
	}
	if !options.RecentlyAdjusted {
		return false
	}
	// During host sleep or VM pause, host wall time advances while guest
	// CLOCK_MONOTONIC stands still. Only continuous guest execution where wall
	// time still diverged from the host indicates an NTP source conflict.
	if !options.LastAdjustedAt.IsZero() && options.LastMonotonicNS > 0 {
		hostElapsed := before.HostTime.Sub(options.LastAdjustedAt)
		guestElapsed := time.Duration(before.MonotonicNS - options.LastMonotonicNS)
		if math.Abs(float64(hostElapsed-guestElapsed)) > float64(Threshold) {
			return false
		}
	}
	return true
}
