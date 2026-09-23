package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/pgsty/farrow/internal/activity"
	"github.com/pgsty/farrow/internal/spec"
	"go.yaml.in/yaml/v3"
)

func TestPlanNamesImpliedDataDisks(t *testing.T) {
	t.Parallel()
	data := spec.Disk{Size: 128 * spec.GiB, Mount: "/data"}
	resolved := spec.Resolved{Nodes: []spec.Node{{Name: "a", Disks: []spec.Disk{data}}, {Name: "b", Disks: []spec.Disk{data}}}}
	if got := planDataDisks(resolved, nil); got != "/data 128 GiB (auto fs) on each node" {
		t.Fatalf("shared disks = %q", got)
	}
	resolved.Nodes[1].Disks = nil
	if got := planDataDisks(resolved, nil); got != "a: /data 128 GiB (auto fs); b: none" {
		t.Fatalf("mixed disks = %q", got)
	}
	if got := planSize(2048 * spec.GiB); got != "2 TiB" {
		t.Fatalf("large size = %q", got)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("fixture write failure")
}

func TestWriterFailureBoundaries(t *testing.T) {
	var stderr bytes.Buffer
	version := runVersionCommand()
	if code := renderCommandOutcome(&version, failingWriter{}, &stderr); code != exitRuntime || !strings.Contains(stderr.String(), "write command output") {
		t.Fatalf("version writer failure code=%d stderr=%q", code, stderr.String())
	}
	if err := confirmDestructive(false, true, "destroy", strings.NewReader("destroy\n"), failingWriter{}); err == nil || !strings.Contains(err.Error(), "confirmation prompt") {
		t.Fatalf("confirmation writer failure = %v", err)
	}
	_, out, _, err := prepareOutput(nil, failingWriter{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	textField(out, 8, "field", "value")
	if outputWriteError(out) == nil {
		t.Fatal("void text-field writer failure was not captured at the root boundary")
	}
	// Diagnostic decoration is deliberately best-effort and must not replace
	// the business result; this call proves a failed diagnostic writer is safe.
	bestEffortf(failingWriter{}, "diagnostic")
}

func TestPrepareOutputDefaultsToTextAndExtractsGlobalFlags(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	args, out, errOut, err := prepareOutput([]string{"--verbose", "status", "--yaml"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(args, " ") != "status" || outputFormatFor(out) != outputYAML || !verboseOutput(errOut) {
		t.Fatalf("prepared args=%v format=%s verbose=%t", args, outputFormatFor(out), verboseOutput(errOut))
	}
	if writerColor(out) || writerColor(errOut) {
		t.Fatal("buffer output unexpectedly enabled ANSI color")
	}
}

func TestPrepareOutputAcceptsVerboseShorthand(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	args, _, errOut, err := prepareOutput([]string{"status", "-v"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(args, " ") != "status" || !verboseOutput(errOut) {
		t.Fatalf("prepared args=%v verbose=%t", args, verboseOutput(errOut))
	}
}

func TestPrepareOutputDoesNotConsumeRemoteArguments(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	args, out, _, err := prepareOutput([]string{"exec", "--json", "--", "tool", "--yaml", "-json"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(args, " "), "exec -- tool --yaml -json"; got != want {
		t.Fatalf("args = %q, want %q", got, want)
	}
	if outputFormatFor(out) != outputJSON {
		t.Fatalf("format = %s, want json", outputFormatFor(out))
	}
}

func TestPrepareOutputRejectsConflictingFormats(t *testing.T) {
	var output bytes.Buffer
	if _, _, _, err := prepareOutput([]string{"status", "--json", "--yaml"}, &output, &output); err == nil {
		t.Fatal("conflicting structured formats were accepted")
	}
}

func TestPrepareOutputYAML(t *testing.T) {
	var output bytes.Buffer
	args, out, _, err := prepareOutput([]string{"--yaml", "version"}, &output, &output)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(args, " "); got != "version" || outputFormatFor(out) != outputYAML {
		t.Fatalf("args=%q format=%s", got, outputFormatFor(out))
	}
}

func TestPresentationFlagsOverrideEnvironment(t *testing.T) {
	t.Setenv("FARROW_OUTPUT", "json")
	t.Setenv("FARROW_VERBOSE", "true")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	args, out, errOut, err := prepareOutput([]string{"version", "--yaml", "--verbose=false"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(args, " "); got != "version" {
		t.Fatalf("args=%q", got)
	}
	if outputFormatFor(out) != outputYAML || verboseOutput(errOut) {
		t.Fatalf("format=%s verbose=%t, want yaml/false", outputFormatFor(out), verboseOutput(errOut))
	}
}

func TestRunRejectsUndocumentedPresentationAliases(t *testing.T) {
	for _, alias := range []string{"--yml", "--yml=true", "-yml", "-yaml", "-json", "-json=false", "-verbose"} {
		t.Run(alias, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			if code := run([]string{"status", alias}, &stdout, &stderr); code != exitUsage {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if stdout.Len() != 0 || !strings.Contains(stderr.String(), "use --json, --yaml, or --verbose") {
				t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestCommandHelpShowsOnlyDocumentedPresentationFlags(t *testing.T) {
	for _, arguments := range [][]string{{"status", "-h"}, {"image", "list", "--help"}} {
		t.Run(strings.Join(arguments, "_"), func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			if code := run(arguments, &stdout, &stderr); code != exitOK {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("help wrote diagnostics to stderr: %q", stderr.String())
			}
			help := stdout.String()
			for _, option := range []string{"--json", "--yaml", "--verbose"} {
				if !strings.Contains(help, option) {
					t.Fatalf("help missing %s:\n%s", option, help)
				}
			}
			for _, alias := range []string{"\n  -json ", "--yml", "\n  -yaml ", "\n  -verbose "} {
				if strings.Contains(help, alias) {
					t.Fatalf("help exposes unsupported alias %q:\n%s", alias, help)
				}
			}
			if !strings.Contains(help, "-v, --verbose") {
				t.Fatalf("help missing verbose shorthand:\n%s", help)
			}
		})
	}
}

func TestEncodeOutputYAMLPreservesJSONFieldNames(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	_, out, _, err := prepareOutput([]string{"status", "--yaml"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	value := struct {
		OperationID string `json:"operation_id"`
		Ready       bool   `json:"ready"`
		Count       int    `json:"count"`
	}{OperationID: "op-1", Ready: true, Count: 123}
	if err := encodeOutput(out, value); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); !strings.Contains(got, "operation_id: op-1") || !strings.Contains(got, "ready: true") || !strings.Contains(got, "count: 123") {
		t.Fatalf("unexpected YAML:\n%s", got)
	}
	var decoded map[string]any
	if err := yaml.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if count, ok := decoded["count"].(int); !ok || count != 123 {
		t.Fatalf("YAML count=%#v (%T), want integer 123", decoded["count"], decoded["count"])
	}
}

func TestJSONAndYAMLDecodeToEquivalentTypedResults(t *testing.T) {
	type result struct {
		OperationID string `json:"operation_id" yaml:"operation_id"`
		Count       int    `json:"count" yaml:"count"`
		Ready       bool   `json:"ready" yaml:"ready"`
	}
	want := result{OperationID: "op-2", Count: 42, Ready: true}
	var decoded []result
	for _, format := range []string{"--json", "--yaml"} {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		_, out, _, err := prepareOutput([]string{"status", format}, &stdout, &stderr)
		if err != nil {
			t.Fatal(err)
		}
		if err := encodeOutput(out, want); err != nil {
			t.Fatal(err)
		}
		var got result
		if format == "--json" {
			err = json.Unmarshal(stdout.Bytes(), &got)
		} else {
			err = yaml.Unmarshal(stdout.Bytes(), &got)
		}
		if err != nil {
			t.Fatalf("decode %s: %v\n%s", format, err, stdout.String())
		}
		decoded = append(decoded, got)
	}
	if decoded[0] != want || decoded[1] != want || decoded[0] != decoded[1] {
		t.Fatalf("json=%+v yaml=%+v want=%+v", decoded[0], decoded[1], want)
	}
}

type failingJSONValue struct{}

func (failingJSONValue) MarshalJSON() ([]byte, error) {
	return nil, errors.New("intentional marshal failure")
}

func TestEncodeOutputIsAtomicOnMarshalFailure(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	_, out, _, err := prepareOutput([]string{"version", "--json"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if err := encodeOutput(out, failingJSONValue{}); err == nil {
		t.Fatal("marshal failure was accepted")
	}
	if stdout.Len() != 0 {
		t.Fatalf("partial structured document leaked: %q", stdout.String())
	}
}

func TestStructuredStdoutStaysPlainWhileTerminalStderrCanUseColor(t *testing.T) {
	state := &outputContext{format: outputJSON, color: true, stdoutTTY: true, stderrTTY: true}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	out := &outputWriter{Writer: &stdout, context: state}
	errOut := &outputWriter{Writer: &stderr, context: state, stderr: true}
	if writerColor(out) {
		t.Fatal("structured stdout enabled ANSI color")
	}
	if !writerColor(errOut) {
		t.Fatal("terminal diagnostics unexpectedly disabled ANSI color")
	}
	if strings.Contains(styled(out, ansiGreen, "ok"), "\x1b[") || !strings.Contains(styled(errOut, ansiGreen, "ok"), "\x1b[") {
		t.Fatal("color did not remain scoped to terminal diagnostics")
	}
}

func TestDiagnosticsAndProgressDoNotPolluteStructuredStdout(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	_, out, errOut, err := prepareOutput([]string{"--verbose", "status", "--json"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	warningf(errOut, "check the selected subnet")
	debugf(errOut, "phase=preflight")
	item := startProgress(context.TODO(), errOut, "Checking readiness")
	item.Stop(nil)
	if err := encodeOutput(out, map[string]any{"ready": true}); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil || decoded["ready"] != true {
		t.Fatalf("stdout=%q decoded=%v err=%v", stdout.String(), decoded, err)
	}
	if strings.Contains(stdout.String(), "WARNING") || strings.Contains(stdout.String(), "debug") || strings.Contains(stdout.String(), "Checking readiness") || strings.Contains(stdout.String(), "\x1b[") {
		t.Fatalf("diagnostics leaked to stdout: %q", stdout.String())
	}
	for _, marker := range []string{"warning:", "debug:", "Checking readiness"} {
		if !strings.Contains(stderr.String(), marker) {
			t.Fatalf("stderr missing %q: %q", marker, stderr.String())
		}
	}
	if strings.Contains(stderr.String(), "\x1b[") {
		t.Fatalf("redirected diagnostics contain ANSI: %q", stderr.String())
	}
}

func TestProgressSourceRedactsCredentialsAndQuery(t *testing.T) {
	t.Parallel()
	got := progressSource("https://build:secret@example.com/catalog.json?token=private#revision")
	if got != "https://example.com/catalog.json" {
		t.Fatalf("redacted source = %q", got)
	}
}

func TestFastProgressUsesNoANSIWhenTerminalStylingIsDisabled(t *testing.T) {
	state := &outputContext{format: outputText, verbose: true, stderrTTY: true, color: false}
	var stderr bytes.Buffer
	errOut := &outputWriter{Writer: &stderr, context: state, stderr: true}
	item := startProgress(context.TODO(), errOut, "Checking readiness")
	item.Stop(nil)
	if strings.Contains(stderr.String(), "\x1b[") {
		t.Fatalf("plain terminal progress contains ANSI: %q", stderr.String())
	}
}

func TestFormatActivityShowsSafeDownloadDetails(t *testing.T) {
	now := time.Date(2026, time.August, 26, 12, 0, 0, 0, time.UTC)
	message := formatActivity(activity.Event{
		Phase:        "image-download",
		Message:      "Downloading image u24 24.04 (arm64)",
		Source:       "https://user:secret@example.test/u24/u24-arm64.qcow2?token=private#fragment",
		CurrentBytes: 256 << 20,
		TotalBytes:   512 << 20,
		StartedAt:    now.Add(-16 * time.Second),
	}, now)
	for _, want := range []string{
		"Downloading image u24 24.04 (arm64)",
		"from https://example.test/u24/u24-arm64.qcow2",
		"256.0 MiB / 512.0 MiB (50.0%)",
		"16.0 MiB/s",
		"ETA 16s",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("activity message missing %q: %q", want, message)
		}
	}
	for _, secret := range []string{"user", "secret", "token", "private", "fragment"} {
		if strings.Contains(message, secret) {
			t.Fatalf("activity message exposed %q: %q", secret, message)
		}
	}
}

func TestProgressBarDistinguishesKnownAndUnknownWork(t *testing.T) {
	if got, want := progressBar(50, 100, 20, 0), "[==========>.........]"; got != want {
		t.Fatalf("determinate progress bar = %q, want %q", got, want)
	}
	if got, want := progressBar(100, 100, 20, 0), "[====================]"; got != want {
		t.Fatalf("complete progress bar = %q, want %q", got, want)
	}
	first := progressBar(0, 0, 20, 0)
	second := progressBar(0, 0, 20, time.Second)
	if first == second || len(first) != 22 || len(second) != 22 || !strings.Contains(first, "====") || !strings.Contains(second, "====") {
		t.Fatalf("indeterminate progress did not move: first=%q second=%q", first, second)
	}
}

func TestProgressReportsDetailedStagesOnlyOnStderr(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	_, out, errOut, err := prepareOutput([]string{"up", "--json", "--verbose"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	item := startProgress(context.TODO(), errOut, "Preparing and starting the deployment")
	item.Report(activity.Event{
		Phase:        "image-download",
		Message:      "Downloading image u24",
		Source:       "https://repo.example/u24.qcow2",
		CurrentBytes: 32 << 20,
		TotalBytes:   64 << 20,
		StartedAt:    time.Now().Add(-2 * time.Second),
	})
	item.Stop(nil)
	if err := encodeOutput(out, map[string]any{"ready": true}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "Downloading") || strings.Contains(stdout.String(), "repo.example") {
		t.Fatalf("progress leaked to structured stdout: %q", stdout.String())
	}
	for _, want := range []string{"Downloading image u24", "repo.example/u24.qcow2", "32.0 MiB / 64.0 MiB (50.0%)"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr missing %q: %q", want, stderr.String())
		}
	}
}

func TestProgressKeepsFastChecksSilent(t *testing.T) {
	state := &outputContext{format: outputText, stderrFile: true, stderrTTY: true, color: true}
	var stderr bytes.Buffer
	item := startProgress(context.TODO(), &outputWriter{Writer: &stderr, context: state, stderr: true}, "Preparing the deployment")
	for range 100 {
		item.Report(activity.Event{Phase: "preflight", Message: "Preflight passed", Done: true})
	}
	item.Stop(nil)
	if stderr.Len() != 0 {
		t.Fatalf("fast checks produced noise: %q", stderr.String())
	}
}

func TestTickfOnlyPrintsInVerboseMode(t *testing.T) {
	state := &outputContext{format: outputText, stderrFile: true, stderrTTY: true}
	var stderr bytes.Buffer
	writer := &outputWriter{Writer: &stderr, context: state, stderr: true}
	tickf(writer, "Network already installed")
	if stderr.Len() != 0 {
		t.Fatalf("fast check printed: %q", stderr.String())
	}
	state.verbose = true
	tickf(writer, "Network already installed")
	if !strings.Contains(stderr.String(), "debug: Network already installed") {
		t.Fatalf("verbose evidence missing: %q", stderr.String())
	}
}

func TestRawWriterUnwrapsPresentationLayer(t *testing.T) {
	var base bytes.Buffer
	state := &outputContext{format: outputText}
	wrapped := &outputWriter{Writer: &outputWriter{Writer: &base, context: state}, context: state}
	if rawWriter(wrapped) != &base {
		t.Fatal("raw writer did not return the underlying passthrough writer")
	}
}

func TestExecuteSSHProcessCapturesStructuredRemoteResult(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	_, out, errOut, err := prepareOutput([]string{"exec", "--json", "--", "ignored"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	result, runErr := executeSSHProcess(
		context.TODO(), "exec", "meta", "dba", "127.0.0.1", 2222,
		"/bin/sh", []string{"-c", "printf remote-out; printf remote-err >&2; exit 7"}, []string{"false"}, out, errOut,
	)
	var exitError *exec.ExitError
	if !errors.As(runErr, &exitError) || exitError.ExitCode() != 7 {
		t.Fatalf("run error=%v, want exit 7", runErr)
	}
	if result.ExitCode != 7 || result.Stdout != "remote-out" || result.Stderr != "remote-err" || result.Success {
		t.Fatalf("result=%+v", result)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("captured command leaked raw output stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestBoundedCaptureReportsTruncation(t *testing.T) {
	capture := boundedCapture{limit: 4}
	if written, err := capture.Write([]byte("abcdef")); err != nil || written != 6 {
		t.Fatalf("write=%d err=%v", written, err)
	}
	if capture.String() != "abcd" || !capture.truncated {
		t.Fatalf("capture=%q truncated=%t", capture.String(), capture.truncated)
	}
}

func TestLogsCobraAcceptsFlagsAfterNode(t *testing.T) {
	command := newLogsCommand(&bytes.Buffer{}, &bytes.Buffer{})
	if err := command.ParseFlags([]string{"meta", "--source", "events", "--follow"}); err != nil {
		t.Fatal(err)
	}
	arguments := command.Flags().Args()
	source, _ := command.Flags().GetString("source")
	follow, _ := command.Flags().GetBool("follow")
	if strings.Join(arguments, " ") != "meta" || source != "events" || !follow {
		t.Fatalf("arguments=%v source=%q follow=%t", arguments, source, follow)
	}
	if err := command.Args(command, []string{"meta", "other"}); err == nil {
		t.Fatal("multiple log nodes were accepted")
	}
}

func TestStructuredLogChunksBoundLongLines(t *testing.T) {
	payload := strings.Repeat("x", structuredLogRecordLimit+5) + "\n"
	reader := bufio.NewReaderSize(strings.NewReader(payload), structuredLogRecordLimit)
	first, continued, err := readStructuredLogChunk(reader)
	if err != nil || !continued || len(first) != structuredLogRecordLimit {
		t.Fatalf("first bytes=%d continued=%t err=%v", len(first), continued, err)
	}
	second, continued, err := readStructuredLogChunk(reader)
	if err != nil || continued || first+second != payload {
		t.Fatalf("second=%q continued=%t err=%v", second, continued, err)
	}
}

func TestStructuredStreamEncodings(t *testing.T) {
	value := logStreamRecord{Type: "line", Node: "meta", Source: "events", Sequence: 1, Content: "ready\n"}
	for _, format := range []string{"--json", "--yaml"} {
		t.Run(format, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			_, out, _, err := prepareOutput([]string{"logs", format, "--follow"}, &stdout, &stderr)
			if err != nil {
				t.Fatal(err)
			}
			if err := encodeStreamOutput(out, value); err != nil {
				t.Fatal(err)
			}
			if format == "--json" {
				var decoded logStreamRecord
				if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil || decoded.Content != value.Content {
					t.Fatalf("NDJSON=%q decoded=%+v err=%v", stdout.String(), decoded, err)
				}
				if strings.Count(stdout.String(), "\n") != 1 {
					t.Fatalf("stream JSON was not one compact line: %q", stdout.String())
				}
			} else {
				if !strings.HasPrefix(stdout.String(), "---\n") {
					t.Fatalf("YAML stream lacks document marker: %q", stdout.String())
				}
				var decoded logStreamRecord
				if err := yaml.Unmarshal(stdout.Bytes(), &decoded); err != nil || decoded.Content != value.Content {
					t.Fatalf("YAML=%q decoded=%+v err=%v", stdout.String(), decoded, err)
				}
			}
		})
	}
}

func TestRunVersionSupportsGlobalStructuredFormats(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "json-before-command", args: []string{"--json", "version"}},
		{name: "yaml-after-command", args: []string{"version", "--yaml"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			if code := run(test.args, &stdout, &stderr); code != exitOK {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
			if strings.Contains(stdout.String(), "\x1b[") {
				t.Fatalf("structured output contains ANSI: %q", stdout.String())
			}
			var decoded map[string]any
			var err error
			if strings.Contains(test.name, "json") {
				err = json.Unmarshal(stdout.Bytes(), &decoded)
			} else {
				err = yaml.Unmarshal(stdout.Bytes(), &decoded)
			}
			if err != nil || decoded["name"] != "farrow" {
				t.Fatalf("output=%q decoded=%v err=%v", stdout.String(), decoded, err)
			}
		})
	}
}

func TestStructuredInteractiveSSHKeepsSessionOffStdout(t *testing.T) {
	for _, format := range []string{"--json", "--yaml"} {
		t.Run(format, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			_, out, errOut, err := prepareOutput([]string{"ssh", format}, &stdout, &stderr)
			if err != nil {
				t.Fatal(err)
			}
			result, runErr := executeSSHProcess(
				context.TODO(), "ssh", "meta", "dba", "127.0.0.1", 2222,
				"/bin/sh", []string{"-c", "printf interactive-session"}, nil, out, errOut,
			)
			if runErr != nil || !result.Interactive || result.SessionStream != "stderr" || result.ExitCode != 0 {
				t.Fatalf("result=%+v err=%v", result, runErr)
			}
			if stdout.Len() != 0 || stderr.String() != "interactive-session" {
				t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			if code := encodeJSON(out, errOut, result); code != exitOK {
				t.Fatalf("encode code=%d stderr=%q", code, stderr.String())
			}
			var decoded map[string]any
			if format == "--json" {
				err = json.Unmarshal(stdout.Bytes(), &decoded)
			} else {
				err = yaml.Unmarshal(stdout.Bytes(), &decoded)
			}
			if err != nil || decoded["command"] != "ssh" || decoded["session_stream"] != "stderr" {
				t.Fatalf("output=%q decoded=%v err=%v", stdout.String(), decoded, err)
			}
		})
	}
}

func TestCompletionStructuredEnvelope(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run([]string{"completion", "bash", "--json"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var result struct {
		Shell  string `json:"shell"`
		Script string `json:"script"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || result.Shell != "bash" || !strings.Contains(result.Script, "__start_farrow") || !strings.Contains(result.Script, "complete -o default") {
		t.Fatalf("result=%+v err=%v output=%s", result, err, stdout.String())
	}
}

func TestLogTextWriterRendersRecordsAndPassesConsoleThrough(t *testing.T) {
	var out bytes.Buffer
	writer := newLogTextWriter(&out, "qemu")
	record := `{"schema":1,"time":"2026-09-23T04:00:00Z","level":"info","node":"meta","operation_id":"12345678","action":"up","message":"started","argv":["/opt/qemu","-name","meta"]}`
	_, _ = writer.Write([]byte(record + "\nqemu-system: raw stderr line\n" + record[:20]))
	writer.Flush()
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || !strings.HasSuffix(lines[0], " info up: started") || strings.Contains(out.String(), "-name") || lines[1] != "qemu-system: raw stderr line" {
		t.Fatalf("rendered log = %q", out.String())
	}
	out.Reset()
	serial := newLogTextWriter(&out, "serial")
	_, _ = serial.Write([]byte(record))
	if out.String() != record {
		t.Fatalf("serial console changed: %q", out.String())
	}
}
