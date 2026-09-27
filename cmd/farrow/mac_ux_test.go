package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pgsty/farrow/internal/macvm"
)

func TestMacHumanSizesPreserveLegacyByteFlags(t *testing.T) {
	for _, tt := range []struct {
		input string
		want  int64
	}{{"16G", 16 << 30}, {"16g", 16 << 30}, {"16GiB", 16 << 30}, {"16gib", 16 << 30}, {"16GB", 16_000_000_000}, {"17179869184", 16 << 30}, {"512M", 512 << 20}, {" 8 GiB ", 8 << 30}} {
		var got int64
		if err := (macSizeValue{&got}).Set(tt.input); err != nil || got != tt.want {
			t.Errorf("%q: got %d, %v", tt.input, got, err)
		}
	}
	for _, input := range []string{"0", "-1", "0G", "2.5G", "9223372036854775807G", "wat"} {
		var got int64
		if err := (macSizeValue{&got}).Set(input); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
}

func TestMacInvalidCPUHasUsageFailureBeforeRunnerLookup(t *testing.T) {
	t.Setenv("FARROW_MAC_RUNNER", "/does/not/exist")
	for _, verb := range []string{"up", "init", "configure"} {
		var out, diagnostics bytes.Buffer
		if code := run([]string{"--json", "mac", verb, "mac2", "--cpu", "0"}, &out, &diagnostics); code != exitUsage || !strings.Contains(out.String(), "at least 2") {
			t.Errorf("%s: %d %s", verb, code, out.String())
		}
	}
}

func TestMacSuccessHintsKeepSelectedSlotAndNoNativeJSON(t *testing.T) {
	for _, tt := range []struct{ state, verb, want string }{{"created", "init", "farrow mac up mac2"}, {"running", "start", "farrow mac up mac2"}, {"stopped", "stop", "farrow mac start mac2"}} {
		var out bytes.Buffer
		outcome := macSlotOutcome(&macvm.Manager{}, &macvm.Slot{Name: "mac2", State: tt.state, Initialized: tt.state == "stopped"}, tt.verb)
		if err := outcome.text(&out, &out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), tt.want) || strings.Contains(out.String(), "mac1") {
			t.Fatalf("misdirected hint: %s", out.String())
		}
	}
	event := macProgressEvent(`{"phase":"restoring","fraction":0.42}`)
	if !strings.Contains(event.Message, "42%") || strings.Contains(event.Message, "{") {
		t.Fatalf("native transport leaked: %+v", event)
	}
}

func TestMacInvalidMemoryAndEmptyConfigureBeforeRunnerLookup(t *testing.T) {
	t.Setenv("FARROW_MAC_RUNNER", "/does/not/exist")
	for _, args := range [][]string{{"up", "--memory", "1G"}, {"init", "--memory", "1G"}, {"configure", "--memory", "1G"}, {"configure"}} {
		var out, diagnostics bytes.Buffer
		if code := run(append([]string{"--json", "mac"}, args...), &out, &diagnostics); code != exitUsage {
			t.Errorf("%v: %d %s", args, code, out.String())
		}
	}
}
