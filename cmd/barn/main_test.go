package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/image"
	darwinnet "github.com/pgsty/barn/internal/network/darwin"
	privatevm "github.com/pgsty/barn/internal/private"
	"github.com/pgsty/barn/internal/spec"
	"github.com/pgsty/barn/internal/sshconfig"
	"github.com/pgsty/barn/internal/sshkeys"
	"github.com/pgsty/barn/internal/state"
)

type recordingDarwinInstaller struct {
	mode  string
	cidr  string
	apply bool
}

func (r *recordingDarwinInstaller) InstallModeNetwork(_ context.Context, _, _, _, mode, cidr string, apply bool) (darwinnet.InstallReport, error) {
	r.mode, r.cidr, r.apply = mode, cidr, apply
	return darwinnet.InstallReport{Action: "install", Applied: apply}, nil
}

func TestDarwinInstallForwardsSharedMode(t *testing.T) {
	recorder := &recordingDarwinInstaller{}
	report, err := installDarwinNetwork(context.TODO(), recorder, "/archive", "uuid", "arm64", "shared", "172.31.251.0/24", true)
	if err != nil || recorder.mode != "shared" || recorder.cidr != "172.31.251.0/24" || !recorder.apply || !report.Applied {
		t.Fatalf("mode=%q cidr=%q apply=%t report=%#v err=%v", recorder.mode, recorder.cidr, recorder.apply, report, err)
	}
}

func TestSortedMapKeysMakesPlanOutputDeterministic(t *testing.T) {
	got := strings.Join(sortedMapKeys(map[string]string{"/z": "z", "/a": "a", "/m": "m"}), ",")
	if got != "/a,/m,/z" {
		t.Fatalf("sorted keys = %q", got)
	}
}

func TestVersion(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "barn dev") {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestInitInvalidCIDRIsUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"init", "full", "--cidr", "not-a-cidr"}, &stdout, &stderr)
	if code != exitUsage || !strings.Contains(stderr.String(), "canonical IPv4 /24") || stdout.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestInitForceOverwritesExistingInventory(t *testing.T) {
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })

	var stdout, stderr bytes.Buffer
	if code := run([]string{"init", "meta"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("initial init code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"init", "full", "--force"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("forced init code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	data, err := os.ReadFile("barn.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("nodename: node-3")) {
		t.Fatalf("--force did not replace the meta inventory with full: %s", data)
	}
}

func TestConfigurationWarningsIncludeNonLoopbackForwards(t *testing.T) {
	t.Parallel()
	resolved := quickResolved(true, false)
	resolved.Nodes[0].Forwards = []spec.Forward{
		{Bind: "127.0.0.1", Host: 15432, Guest: 5432, Protocol: "tcp"},
		{Bind: "0.0.0.0", Host: 13000, Guest: 3000, Protocol: "tcp"},
	}
	warnings := configurationWarnings(resolved)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "exposes host TCP 0.0.0.0:13000 beyond loopback") || strings.Contains(warnings[0], "WARNING") {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestTestingImageWarningIsProminent(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	printImageStatusWarning(&output, image.Entry{Alias: "u24", Arch: "arm64", Release: "test", Status: "testing"})
	if !strings.Contains(output.String(), "warning: image u24/arm64") || !strings.Contains(output.String(), "status testing, not supported") {
		t.Fatalf("warning = %q", output.String())
	}
	output.Reset()
	printImageStatusWarning(&output, image.Entry{Alias: "u24", Status: "supported"})
	if output.Len() != 0 {
		t.Fatalf("supported image warning = %q", output.String())
	}
	printImageStatusWarning(&output, image.Entry{Alias: "el7", Arch: "amd64", Release: "7.9", Status: "deprecated"})
	if !strings.Contains(output.String(), "deprecated and EOL") {
		t.Fatalf("deprecated image warning = %q", output.String())
	}
}

func TestLifecycleImageArchUsesResolvedGuest(t *testing.T) {
	t.Parallel()
	if got := lifecycleImageArch(spec.Resolved{Arch: "amd64"}); got != "amd64" {
		t.Fatalf("explicit lifecycle image arch = %q", got)
	}
	if got := lifecycleImageArch(spec.Resolved{}); got != runtime.GOARCH {
		t.Fatalf("native lifecycle image arch = %q, want %q", got, runtime.GOARCH)
	}
}

func TestImplicitSetupKeepsUpRepositorySelection(t *testing.T) {
	t.Setenv("BARN_REPO", "https://environment.example/barn")
	repository, _, err := image.ResolveRepository("", true)
	if err != nil {
		t.Fatal(err)
	}
	options := lifecycleOptions{ConfigPath: "/tmp/barn.yml", Mirror: true}
	setupOptions := implicitSetupOptions(options, repository)
	if repository != image.ChinaRepositoryURL || setupOptions.Repo != repository || setupOptions.FilePath != options.ConfigPath || setupOptions.Mode != "host" {
		t.Fatalf("implicit setup options = %#v from repository %q", setupOptions, repository)
	}
}

func TestDestructiveConfirmationTTYAndNonTTY(t *testing.T) {
	t.Parallel()
	var prompt bytes.Buffer
	if err := confirmDestructive(false, true, "destroy", strings.NewReader("destroy\n"), &prompt); err != nil {
		t.Fatal(err)
	}
	if prompt.String() != `Type "destroy" to confirm: ` {
		t.Fatalf("prompt = %q", prompt.String())
	}
	if err := confirmDestructive(false, true, "destroy", strings.NewReader("destory\n"), io.Discard); !errors.Is(err, ErrCancelled) || !strings.Contains(err.Error(), `typed "destory", expected "destroy"; nothing was changed`) {
		t.Fatalf("mismatched interactive confirmation error = %v, want ErrCancelled", err)
	}
	if err := confirmDestructive(false, false, "destroy", strings.NewReader("destroy\n"), io.Discard); err == nil || !strings.Contains(err.Error(), "requires --force") {
		t.Fatalf("non-TTY confirmation error = %v", err)
	}
	if err := confirmDestructive(true, false, "destroy", strings.NewReader(""), io.Discard); err != nil {
		t.Fatalf("--force rejected: %v", err)
	}
}

func TestRollbackFlagScope(t *testing.T) {
	t.Setenv("BARN_HOME", t.TempDir())
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	for _, command := range []string{"plan", "recreate"} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{command, "--rollback"}, &stdout, &stderr); code != exitUsage || !strings.Contains(stderr.String(), "unknown flag") {
			t.Fatalf("%s code=%d stdout=%q stderr=%q", command, code, stdout.String(), stderr.String())
		}
	}
	for _, command := range []string{"up", "reload"} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{command, "--rollback"}, &stdout, &stderr); code != exitUsage || !strings.Contains(stderr.String(), "no inventory found") {
			t.Fatalf("configless %s code=%d stdout=%q stderr=%q", command, code, stdout.String(), stderr.String())
		}
	}
}

func TestProvisionRejectsUnsafeInputBeforeDeploymentAccess(t *testing.T) {
	root := t.TempDir()
	scriptPath := filepath.Join(root, "setup.sh")
	if err := os.WriteFile(scriptPath, []byte("echo ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(root, "setup-link.sh")
	if err := os.Symlink(scriptPath, symlinkPath); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		options provisionOptions
		code    int
		want    string
	}{
		{name: "missing script flag", options: provisionOptions{Parallelism: 1, Timeout: time.Hour}, code: exitUsage, want: "--script is required"},
		{name: "missing script file", options: provisionOptions{ScriptPath: filepath.Join(root, "missing.sh"), Parallelism: 1, Timeout: time.Hour}, code: exitUsage, want: "missing.sh does not exist"},
		{name: "invalid parallelism", options: provisionOptions{ScriptPath: scriptPath, Parallelism: 0, Timeout: time.Hour}, code: exitUsage, want: "parallelism must be"},
		{name: "invalid timeout", options: provisionOptions{ScriptPath: scriptPath, Parallelism: 1, Timeout: 25 * time.Hour}, code: exitUsage, want: "no more than 24h"},
		{name: "symlink script", options: provisionOptions{ScriptPath: symlinkPath, Parallelism: 1, Timeout: time.Hour}, code: exitUsage, want: "non-symlink"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			outcome, runErr := runProvision(context.TODO(), test.options, nil, &stderr)
			code := exitOK
			if runErr != nil {
				typed, ok := runErr.(typedCommandError)
				if !ok {
					t.Fatalf("untyped provision error: %v", runErr)
				}
				code = renderTypedCommandError(typed, &stdout, &stderr)
			} else {
				code = renderCommandOutcome(&outcome, &stdout, &stderr)
			}
			if code != test.code || !strings.Contains(stderr.String(), test.want) || stdout.Len() != 0 {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestProvisionConnectionExitClassifiesSSHArtifactIntegrity(t *testing.T) {
	if code := provisionConnectionExit(&sshkeys.SSHArtifactError{Err: errors.New("unsafe fixture")}); code != exitIntegrity {
		t.Fatalf("SSH artifact error exit = %d", code)
	}
	if code := provisionConnectionExit(errors.New("runtime identity mismatch")); code != exitRuntime {
		t.Fatalf("runtime error exit = %d", code)
	}
}

func TestCommandTimeoutHonorsResolvedReadiness(t *testing.T) {
	t.Parallel()
	resolved := quickResolved(true, true)
	if got := withReadinessTimeout(10*time.Minute, resolved); got != 10*time.Minute {
		t.Fatalf("default command timeout = %s", got)
	}
	resolved.SSHWaitTimeoutNS = int64(20 * time.Minute)
	if got := withReadinessTimeout(10*time.Minute, resolved); got != 25*time.Minute {
		t.Fatalf("extended command timeout = %s", got)
	}
	resolved.SSHWaitTimeoutNS = -1
	if got := withReadinessTimeout(10*time.Minute, resolved); got != 10*time.Minute {
		t.Fatalf("invalid timeout changed command deadline: %s", got)
	}
}

func TestSplitRemoteInvocationValidatesNodesBeforeSeparator(t *testing.T) {
	t.Parallel()
	resolved := spec.Resolved{Nodes: []spec.Node{{Name: "meta"}, {Name: "node-1"}}}
	for _, test := range []struct {
		arguments []string
		node      string
		command   string
		implicit  bool
		wantErr   string
	}{
		{arguments: nil},
		{arguments: []string{"meta"}, node: "meta"},
		{arguments: []string{"meta", "uptime"}, node: "meta", command: "uptime"},
		{arguments: []string{"uptime", "-p"}, command: "uptime -p", implicit: true},
		{arguments: []string{"--", "uptime"}, command: "uptime"},
		{arguments: []string{"meta", "--", "ls", "--", "-l"}, node: "meta", command: "ls -- -l"},
		{arguments: []string{"metaa", "--", "uptime"}, wantErr: `the deployment has no node "metaa"`},
		{arguments: []string{"meta", "node-1", "--", "uptime"}, wantErr: "at most one node may precede --"},
		{arguments: []string{"node-2", "uptime"}, wantErr: `unknown node "node-2" (did you mean node-1?)`},
		{arguments: []string{"--", "node-2"}, command: "node-2"},
		{arguments: []string{"node", "-e", "1"}, command: "node -e 1", implicit: true},
	} {
		node, command, implicit, err := splitRemoteInvocation(test.arguments, resolved)
		if test.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Errorf("split(%v) err = %v, want %q", test.arguments, err, test.wantErr)
			}
			continue
		}
		if err != nil || node != test.node || strings.Join(command, " ") != test.command || implicit != test.implicit {
			t.Errorf("split(%v) = %q, %q, implicit=%t, %v; want %q, %q, implicit=%t", test.arguments, node, strings.Join(command, " "), implicit, err, test.node, test.command, test.implicit)
		}
	}
	for _, arguments := range [][]string{
		{"__complete", "init", "--cidr", ""},
		{"__complete", "recreate", "--force="},
		{"__complete", "provision", "--parallel", ""},
		{"__complete", "ssh-config", "--name", ""},
		{"__complete", "image", "import", "--sha256", ""},
		{"__complete", "network", "install", "--interface-id", ""},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(arguments, &stdout, &stderr); code != exitOK {
			t.Fatalf("scalar completion %v code=%d stderr=%s", arguments, code, stderr.String())
		}
		if !strings.HasSuffix(strings.TrimSpace(stdout.String()), ":4") {
			t.Errorf("scalar completion %v did not suppress file names: %q", arguments, stdout.String())
		}
	}
	var pathStdout, pathStderr bytes.Buffer
	if code := run([]string{"__complete", "init", "--output", ""}, &pathStdout, &pathStderr); code != exitOK {
		t.Fatalf("path completion code=%d stderr=%s", code, pathStderr.String())
	}
	if !strings.HasSuffix(strings.TrimSpace(pathStdout.String()), ":0") {
		t.Errorf("path completion unexpectedly suppressed file names: %q", pathStdout.String())
	}
}

func TestImplicitRemoteCommandWarnsOnceAtTheCommandBoundary(t *testing.T) {
	t.Setenv("BARN_HOME", t.TempDir())
	resolved := spec.Resolved{Nodes: []spec.Node{{Name: "meta", Control: true}}}
	for _, test := range []struct {
		name        string
		commandName string
		arguments   []string
		want        string
	}{
		{name: "ssh heuristic", commandName: "ssh", arguments: []string{"uptime"}, want: `warning: treating "uptime" as a remote command; write barn ssh [node] -- command`},
		{name: "exec heuristic", commandName: "exec", arguments: []string{"hostname"}, want: `warning: treating "hostname" as a remote command; write barn exec [node] -- command`},
		{name: "known node", commandName: "ssh", arguments: []string{"meta", "uptime"}},
		{name: "explicit separator", commandName: "ssh", arguments: []string{"--", "uptime"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			_, _ = runPrivateSSH(context.TODO(), test.commandName, test.arguments, resolved, io.Discard, &stderr)
			if got := strings.TrimSpace(stderr.String()); got != test.want {
				t.Fatalf("stderr=%q want=%q", got, test.want)
			}
		})
	}
}

func TestNodeSelectorsAreValidatedBeforeAnyOperation(t *testing.T) {
	resolved := spec.Resolved{Nodes: []spec.Node{{Name: "meta"}, {Name: "node-1"}}}
	if err := validateNodeSelectors(resolved, []string{"node-1", "meta"}, "inventory"); err != nil {
		t.Fatal(err)
	}
	if err := validateNodeSelectors(resolved, []string{"meta", "definitely-not-a-node"}, "inventory"); err == nil || !strings.Contains(err.Error(), `no node "definitely-not-a-node"`) {
		t.Fatalf("unknown selector err = %v", err)
	}
	if err := validateNodeSelectors(resolved, []string{"meta", "meta"}, "inventory"); err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("duplicate selector err = %v", err)
	}
	// Every inventory-reading command rejects the selector as a usage error
	// before host preflight or a confirmation prompt; recreate would
	// otherwise demand --force first, and up/plan would probe the network.
	t.Setenv("BARN_HOME", t.TempDir())
	for _, command := range []string{"plan", "up", "reload", "recreate"} {
		var stdout, stderr bytes.Buffer
		code := run([]string{"--json", command, "-f", "../../tests/fixtures/private-two.yaml", "definitely-not-a-node"}, &stdout, &stderr)
		if code != exitUsage || !strings.Contains(stdout.String(), `"error": "usage"`) || !strings.Contains(stdout.String(), "definitely-not-a-node") {
			t.Errorf("%s unknown node code=%d stdout=%s stderr=%s", command, code, stdout.String(), stderr.String())
		}
	}
}

func TestDestroyScopeStatesWhatIsRemoved(t *testing.T) {
	t.Parallel()
	resolved := spec.Resolved{Nodes: []spec.Node{{Name: "meta"}, {Name: "node-1"}}}
	if got := destroyScope(resolved, []string{"node-1"}, false, false); !strings.Contains(got, "node(s) node-1 are removed") {
		t.Fatalf("node scope = %q", got)
	}
	whole := destroyScope(resolved, nil, false, false)
	if !strings.Contains(whole, "2 node(s): meta, node-1") || !strings.Contains(whole, "are preserved") {
		t.Fatalf("whole scope = %q", whole)
	}
	if got := destroyScope(resolved, nil, true, false); !strings.Contains(got, "also deletes persistent data disks (keys and state are preserved)") {
		t.Fatalf("delete-persistent scope = %q", got)
	}
	if got := destroyScope(resolved, nil, true, true); !strings.Contains(got, "deployment keys, and the deployment state") {
		t.Fatalf("purge scope = %q", got)
	}
}

type recordingSSHConfigReconciler struct {
	installCalls int
	removeCalls  int
	name         string
	home         string
	err          error
}

func (reconciler *recordingSSHConfigReconciler) InstallSSHConfig(_ context.Context, name, home string) (sshconfig.Result, error) {
	reconciler.installCalls++
	reconciler.name = name
	reconciler.home = home
	return sshconfig.Result{Action: "install", Fragment: "/tmp/barn_config", Changed: true}, reconciler.err
}

func (reconciler *recordingSSHConfigReconciler) RemoveSSHConfig(name, home string) (sshconfig.Result, error) {
	reconciler.removeCalls++
	reconciler.name = name
	reconciler.home = home
	return sshconfig.Result{Action: "remove", Fragment: "/tmp/barn_config", Changed: true}, reconciler.err
}

func TestLifecycleSSHConfigReconciliationPolicy(t *testing.T) {
	for _, test := range []struct {
		command            string
		deploymentHasNodes bool
		action             string
	}{
		{command: "start", deploymentHasNodes: true, action: "install"},
		{command: "up", deploymentHasNodes: true, action: "install"},
		{command: "reload", deploymentHasNodes: true, action: "install"},
		{command: "recreate", deploymentHasNodes: true, action: "install"},
		{command: "destroy", deploymentHasNodes: true, action: "install"},
		{command: "destroy", deploymentHasNodes: false, action: "remove"},
		{command: "purge", deploymentHasNodes: false, action: "remove"},
	} {
		t.Run(test.command+"_"+test.action, func(t *testing.T) {
			reconciler := &recordingSSHConfigReconciler{}
			result, err := reconcileLifecycleSSHConfig(context.TODO(), test.command, test.deploymentHasNodes, reconciler)
			if err != nil {
				t.Fatal(err)
			}
			if test.action == "" {
				if result != nil || reconciler.installCalls != 0 || reconciler.removeCalls != 0 {
					t.Fatalf("result=%#v reconciler=%#v", result, reconciler)
				}
				return
			}
			if result == nil || result.Action != test.action || reconciler.name != "barn" || reconciler.home != "" {
				t.Fatalf("result=%#v reconciler=%#v", result, reconciler)
			}
			if test.action == "install" && (reconciler.installCalls != 1 || reconciler.removeCalls != 0) {
				t.Fatalf("install reconciler=%#v", reconciler)
			}
			if test.action == "remove" && (reconciler.removeCalls != 1 || reconciler.installCalls != 0) {
				t.Fatalf("remove reconciler=%#v", reconciler)
			}
		})
	}
}

func TestDestroySSHPolicyUsesThePreOperationNodeSet(t *testing.T) {
	resolved := spec.Resolved{Nodes: []spec.Node{{Name: "meta"}, {Name: "node-1"}}}
	for _, test := range []struct {
		selected []string
		want     bool
	}{
		{selected: nil, want: false},
		{selected: []string{"meta"}, want: true},
		{selected: []string{"meta", "node-1"}, want: false},
	} {
		if got := destroyLeavesDeploymentNodes(resolved, test.selected); got != test.want {
			t.Errorf("selected=%v leaves nodes=%t, want %t", test.selected, got, test.want)
		}
	}
}

func TestLifecycleSSHConfigAlwaysUsesTheFullDeployment(t *testing.T) {
	manager := fullDeploymentSSHManager(privatevm.Manager{Nodes: []string{"meta"}})
	if len(manager.Nodes) != 0 {
		t.Fatalf("full deployment manager retained selectors: %v", manager.Nodes)
	}
}

func TestSelectedUpReconciliationKeepsEveryDeploymentSSHEntry(t *testing.T) {
	root := t.TempDir()
	dataRoot := filepath.Join(root, "data")
	home := filepath.Join(root, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BARN_HOME", dataRoot)
	t.Setenv("HOME", home)
	resolved := spec.Resolved{
		Schema: 1, Name: "private", Image: "u24", Network: "private", SSHUser: "dba",
		Private: &spec.PrivateNetwork{CIDR: "10.10.10.0/24", HostAddress: "10.10.10.1", DHCPEnd: "10.10.10.8"},
		Nodes: []spec.Node{
			{Name: "meta", Control: true, Address: "10.10.10.10", CPUs: 1, Memory: 2 * spec.GiB, RootDisk: 8 * spec.GiB},
			{Name: "node-1", Address: "10.10.10.11", CPUs: 1, Memory: 2 * spec.GiB, RootDisk: 8 * spec.GiB},
		},
	}
	hash, err := spec.Hash(resolved)
	if err != nil {
		t.Fatal(err)
	}
	store := state.Store{Root: dataRoot}
	now := time.Now().UTC()
	if err := store.WriteDeployment(state.DeploymentState{Schema: state.DeploymentSchema, BarnVersion: "test", SpecHash: hash, Resolved: resolved, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	nodeHashes, err := spec.NodeHashes(resolved)
	if err != nil {
		t.Fatal(err)
	}
	for index, node := range resolved.Nodes {
		if err := store.WriteNode(state.NodeState{
			Schema: state.NodeSchema, BarnVersion: "test", Node: node.Name, VMUUID: "uuid-" + node.Name,
			Phase: state.Stopped, Generation: 1, SpecHash: nodeHashes[node.Name], SSHPort: uint16(2222 + index), CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	keysDir := filepath.Join(dataRoot, "keys")
	if err := os.Mkdir(keysDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"id_ed25519", "known_hosts"} {
		if err := os.WriteFile(filepath.Join(keysDir, name), []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	selected := fullDeploymentSSHManager(privatevm.Manager{BarnVersion: "test", Nodes: []string{"meta"}})
	result, err := reconcileLifecycleSSHConfig(context.TODO(), "up", true, selected)
	if err != nil {
		t.Fatal(err)
	}
	fragment, err := os.ReadFile(result.Fragment)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{"barn-meta meta", "barn-node-1 node-1"} {
		if !strings.Contains(string(fragment), entry) {
			t.Fatalf("selected up reconciliation dropped %q:\n%s", entry, fragment)
		}
	}
	remaining := resolved
	remaining.Nodes = append([]spec.Node(nil), resolved.Nodes[:1]...)
	remainingHash, err := spec.Hash(remaining)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteDeployment(state.DeploymentState{Schema: state.DeploymentSchema, BarnVersion: "test", SpecHash: remainingHash, Resolved: remaining, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	result, err = reconcileLifecycleSSHConfig(context.TODO(), "destroy", true, selected)
	if err != nil {
		t.Fatal(err)
	}
	fragment, err = os.ReadFile(result.Fragment)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(fragment), "barn-meta meta") || strings.Contains(string(fragment), "node-1") {
		t.Fatalf("node destroy did not reconcile the remaining deployment:\n%s", fragment)
	}
	result, err = reconcileLifecycleSSHConfig(context.TODO(), "destroy", false, selected)
	if err != nil || result.Action != "remove" {
		t.Fatalf("whole destroy reconciliation result=%#v err=%v", result, err)
	}
	if _, err := os.Lstat(result.Fragment); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("whole destroy left the default SSH fragment: %v", err)
	}
}

func TestLifecycleSSHConfigFailurePreservesThePartialResult(t *testing.T) {
	reconciler := &recordingSSHConfigReconciler{err: errors.New("unsafe SSH directory")}
	result, err := reconcileLifecycleSSHConfig(context.TODO(), "up", true, reconciler)
	if err == nil || result == nil || result.Action != "install" || !strings.Contains(err.Error(), "unsafe SSH directory") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestLifecycleIntegrationWarningKeepsSuccessfulExitAndStructuredDetails(t *testing.T) {
	var stdout, stderr bytes.Buffer
	_, out, errOut, err := prepareOutput([]string{"--json"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	result := lifecycleResult{Status: privatevm.Status{Message: "created and started"}, Warnings: []lifecycleWarning{sshIntegrationWarning("install", errors.New("unsafe SSH directory"))}}
	outcome := commandOutcome{payload: result}
	if code := renderCommandOutcome(&outcome, out, errOut); code != exitOK {
		t.Fatalf("optional integration failed the operation: %d", code)
	}
	var decoded lifecycleResult
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil || len(decoded.Warnings) != 1 || decoded.Warnings[0].Detail != "unsafe SSH directory" || stderr.Len() != 0 {
		t.Fatalf("warning lost or JSON polluted: %s %s %v", stdout.String(), stderr.String(), err)
	}
	if warning := sshIntegrationWarning("remove", errors.New("fixture")); warning.Next != "barn ssh-config --remove" {
		t.Fatalf("wrong purge recovery: %+v", warning)
	}
}

func TestLifecyclePartialFailurePreservesPerNodeDetails(t *testing.T) {
	partial := &privatevm.PartialError{
		Total: 1,
		Failures: []privatevm.NodeFailure{{
			Node: "d12-1", Stage: "readiness", Error: "guest bootstrap failed during data-disks",
		}},
	}
	wrapped := fmt.Errorf("%w; SSH client configuration reconciliation also failed: unsafe fragment", partial)
	typed, ok := classifyPrivateLifecycleError(wrapped, "op-1").(typedCommandError)
	if !ok {
		t.Fatal("partial failure was not typed")
	}
	payload, ok := typed.commandPayload().(lifecyclePartialFailure)
	if !ok || len(payload.Failures) != 1 || payload.Failures[0].Stage != "readiness" || payload.OperationID != "op-1" || !strings.Contains(payload.Message, "unsafe fragment") {
		t.Fatalf("partial payload = %#v", typed.commandPayload())
	}
}

func TestImageListJSON(t *testing.T) {
	t.Setenv("BARN_HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := run([]string{"image", "list", "--json"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	for _, want := range []string{`"alias": "u24"`, `"arch": "arm64"`, `"channels": [`, `"stable"`, `"status": "supported"`} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("image JSON missing %q: %s", want, stdout.String())
		}
	}
	for _, want := range []string{`"alias": "el7"`, `"alias": "el8"`} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("restored compatibility image %s missing from embedded image list: %s", want, stdout.String())
		}
	}
}

func TestUsageErrors(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != exitOK || !strings.Contains(stdout.String(), "barn --help") {
		t.Fatalf("empty invocation code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, args := range [][]string{{"unknown"}, {"version", "extra"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != exitUsage {
			t.Errorf("run(%v) code=%d, want %d", args, code, exitUsage)
		}
	}
}

func TestDeletePersistentRequiresDestroyAndForce(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"up", "--delete-persistent"},
		{"recreate", "--force", "--delete-persistent"},
		{"destroy", "--delete-persistent"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != exitUsage {
			t.Errorf("run(%v) code=%d stderr=%s, want usage", args, code, stderr.String())
		}
	}
}

func TestNoWaitOnlyAppliesToStartingCommands(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"plan", "--no-wait"},
		{"status", "--no-wait"},
		{"stop", "--no-wait"},
		{"destroy", "--force", "--no-wait"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != exitUsage {
			t.Errorf("run(%v) code=%d stderr=%s, want usage", args, code, stderr.String())
		}
	}
}

func TestCompletions(t *testing.T) {
	t.Setenv("BARN_HOME", t.TempDir())
	for _, shell := range []string{"bash", "zsh", "fish"} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"completion", shell}, &stdout, &stderr); code != exitOK {
			t.Fatalf("%s completion code=%d stderr=%s", shell, code, stderr.String())
		}
		for _, want := range []string{"barn", "__barn"} {
			if !strings.Contains(stdout.String(), want) {
				t.Errorf("%s completion missing %q: %s", shell, want, stdout.String())
			}
		}
	}
	for _, test := range []struct {
		args []string
		want []string
	}{
		{args: []string{"__complete", "setup", ""}, want: []string{"meta", "dual", "trio", "full"}},
		{args: []string{"__complete", "setup", "--mode", ""}, want: []string{"host", "shared"}},
		{args: []string{"__complete", "image", ""}, want: []string{"list", "pull", "sync"}},
		{args: []string{"__complete", "image", "pull", "u"}, want: []string{"u24", "u24:stable", "u24@20260911.0.0", "u24@20260801.0.0"}},
		{args: []string{"__complete", "up", "--file", "../../tests/fixtures/private-two.yaml", ""}, want: []string{"meta", "node-1"}},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(test.args, &stdout, &stderr); code != exitOK {
			t.Fatalf("dynamic completion %v code=%d stderr=%s", test.args, code, stderr.String())
		}
		for _, want := range test.want {
			if !strings.Contains(stdout.String(), want) {
				t.Errorf("dynamic completion %v missing %q: %s", test.args, want, stdout.String())
			}
		}
	}
}

func TestDeploymentDestroyUsesPersistedStateWithoutConfiguration(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BARN_HOME", root)
	resolved := spec.Resolved{
		Schema: 1, Name: "private", Image: "u24", Network: "private", SSHUser: "dba",
		Private: &spec.PrivateNetwork{CIDR: "10.10.10.0/24", HostAddress: "10.10.10.1", DHCPEnd: "10.10.10.8"},
		Nodes:   []spec.Node{{Name: "meta", Control: true, Address: "10.10.10.10", CPUs: 1, Memory: 2 * spec.GiB, RootDisk: 8 * spec.GiB}},
	}
	hash, err := spec.Hash(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if err := (state.Store{Root: root}).WriteDeployment(state.DeploymentState{Schema: state.DeploymentSchema, BarnVersion: "test", SpecHash: hash, Resolved: resolved, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	var stdout, stderr bytes.Buffer
	if code := run([]string{"destroy"}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "destroy requires --force") {
		t.Fatalf("deployment destroy dispatch was ambiguous: %s", stderr.String())
	}
}

func TestPurgeIsIdempotentWithoutDeploymentAndNeedsNoConfirmation(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "missing-barn-home")
	home := filepath.Join(parent, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BARN_HOME", root)
	t.Setenv("HOME", home)
	for _, invocation := range [][]string{{"purge", "--json"}, {"purge", "--json"}} {
		var stdout, stderr bytes.Buffer
		if code := run(invocation, &stdout, &stderr); code != exitOK {
			t.Fatalf("run(%v) code=%d stdout=%q stderr=%q", invocation, code, stdout.String(), stderr.String())
		}
		var result lifecycleResult
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
			t.Fatalf("run(%v) result JSON: %v\n%s", invocation, err, stdout.String())
		}
		if !strings.Contains(result.Message, "no deployment to purge") || strings.Contains(strings.ToLower(stderr.String()), "confirm") {
			t.Fatalf("run(%v) result=%#v stderr=%q", invocation, result, stderr.String())
		}
		if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("run(%v) created missing data root: %v", invocation, err)
		}
		if _, err := os.Lstat(filepath.Join(home, ".ssh")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("run(%v) created an SSH directory for a no-op purge: %v", invocation, err)
		}
	}
}

func TestPurgeRejectsNodeSelectors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"purge", "meta", "--json"}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var failure commandFailure
	if err := json.Unmarshal(stdout.Bytes(), &failure); err != nil {
		t.Fatalf("failure JSON: %v\n%s", err, stdout.String())
	}
	if failure.Error != "usage" || !strings.Contains(failure.Message, "unknown command") && !strings.Contains(failure.Message, "accepts 0 arg") {
		t.Fatalf("failure=%#v stderr=%q", failure, stderr.String())
	}
}

func TestPurgeDisposesAppliedDeploymentWithoutConfirmation(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("BARN_HOME", root)
	t.Setenv("HOME", home)
	resolved := spec.Resolved{
		Schema: 1, Name: "barn", Image: "d13", Network: "private", SSHUser: "dba",
		Private: &spec.PrivateNetwork{CIDR: "10.10.10.0/24", HostAddress: "10.10.10.1", DHCPEnd: "10.10.10.8"},
		Nodes:   []spec.Node{{Name: "meta", Control: true, Address: "10.10.10.10", CPUs: 2, Memory: 4 * spec.GiB, RootDisk: 64 * spec.GiB}},
	}
	hash, err := spec.Hash(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if err := (state.Store{Root: root}).WriteDeployment(state.DeploymentState{
		Schema: state.DeploymentSchema, BarnVersion: "test", SpecHash: hash,
		Resolved: resolved, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(root, "keys")
	if err := os.Mkdir(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, mode := range map[string]os.FileMode{
		"id_ed25519": 0o600, "id_ed25519.pub": 0o644, "known_hosts": 0o600,
	} {
		if err := os.WriteFile(filepath.Join(keys, name), []byte("fixture"), mode); err != nil {
			t.Fatal(err)
		}
	}
	image := filepath.Join(root, "images", "fixture.qcow2")
	if err := os.MkdirAll(filepath.Dir(image), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(image, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"purge", "--json"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if strings.Contains(strings.ToLower(stderr.String()), "confirm") {
		t.Fatalf("purge asked for confirmation: %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "keys, and persistent data disks preserved") || strings.Contains(stdout.String(), "keys preserved") {
		t.Fatalf("purge reports resources as both preserved and deleted: %s", stdout.String())
	}
	for _, removed := range []string{filepath.Join(root, "state.json"), keys} {
		if _, err := os.Lstat(removed); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("purge left %s: %v", removed, err)
		}
	}
	if content, err := os.ReadFile(image); err != nil || string(content) != "image" {
		t.Fatalf("purge changed cached image: content=%q err=%v", content, err)
	}
}

func TestPurgeCleansResidualKeysButPreservesImages(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("BARN_HOME", root)
	t.Setenv("HOME", home)
	keys := filepath.Join(root, "keys")
	if err := os.Mkdir(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, mode := range map[string]os.FileMode{
		"id_ed25519": 0o600, "id_ed25519.pub": 0o644, "known_hosts": 0o600,
	} {
		if err := os.WriteFile(filepath.Join(keys, name), []byte("fixture"), mode); err != nil {
			t.Fatal(err)
		}
	}
	image := filepath.Join(root, "images", "fixture.qcow2")
	if err := os.MkdirAll(filepath.Dir(image), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(image, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"purge", "--json"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Lstat(keys); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("purge left residual keys: %v", err)
	}
	if content, err := os.ReadFile(image); err != nil || string(content) != "image" {
		t.Fatalf("purge changed cached image: content=%q err=%v", content, err)
	}
}

func TestPurgeRefusesNodeArtifactsWithoutDeploymentState(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("BARN_HOME", root)
	t.Setenv("HOME", home)
	nodeDir := filepath.Join(root, "nodes", "meta")
	if err := os.MkdirAll(nodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(nodeDir, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"purge", "--json"}, &stdout, &stderr); code != exitIntegrity {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var failure commandFailure
	if err := json.Unmarshal(stdout.Bytes(), &failure); err != nil {
		t.Fatalf("failure JSON: %v\n%s", err, stdout.String())
	}
	if failure.Error != "integrity" || !strings.Contains(failure.Message, "state is missing") {
		t.Fatalf("failure=%#v stderr=%q", failure, stderr.String())
	}
	if content, err := os.ReadFile(sentinel); err != nil || string(content) != "keep" {
		t.Fatalf("unsafe residual artifact was changed: content=%q err=%v", content, err)
	}
}

func TestDeploymentEventLogRejectsNodeSelector(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BARN_HOME", root)
	resolved := spec.Resolved{
		Schema: 1, Name: "private", Image: "u24", Network: "private", SSHUser: "dba",
		Private: &spec.PrivateNetwork{CIDR: "10.10.10.0/24", HostAddress: "10.10.10.1", DHCPEnd: "10.10.10.8"},
		Nodes:   []spec.Node{{Name: "meta", Control: true, Address: "10.10.10.10", CPUs: 1, Memory: 2 * spec.GiB, RootDisk: 8 * spec.GiB}},
	}
	hash, err := spec.Hash(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if err := (state.Store{Root: root}).WriteDeployment(state.DeploymentState{
		Schema: state.DeploymentSchema, BarnVersion: "test", SpecHash: hash,
		Resolved: resolved, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--json", "logs", "meta", "--source", "events"}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var failure commandFailure
	if err := json.Unmarshal(stdout.Bytes(), &failure); err != nil {
		t.Fatalf("failure JSON: %v\n%s", err, stdout.String())
	}
	if failure.Error != "usage" || !strings.Contains(failure.Message, "deployment-wide event log") {
		t.Fatalf("failure=%#v stderr=%q", failure, stderr.String())
	}
}

func TestDeploymentDiagnosticsRejectUnsupportedNetwork(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BARN_HOME", root)
	resolved := quickResolved(true, false)
	resolved.Network = "unsupported"
	resolved.Private = nil
	hash, err := spec.Hash(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if err := (state.Store{Root: root}).WriteDeployment(state.DeploymentState{
		Schema: state.DeploymentSchema, BarnVersion: "test", SpecHash: hash,
		Resolved: resolved, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"logs", "ssh-config", "status", "ssh"} {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		if code := run([]string{"--json", command}, &stdout, &stderr); code != exitConflict {
			t.Fatalf("%s code=%d stdout=%q stderr=%q", command, code, stdout.String(), stderr.String())
		}
		var failure commandFailure
		if err := json.Unmarshal(stdout.Bytes(), &failure); err != nil {
			t.Fatalf("%s failure JSON: %v\n%s", command, err, stdout.String())
		}
		if failure.Error != "conflict" || failure.Message != invalidDeploymentNetworkMessage || !strings.Contains(stderr.String(), invalidDeploymentNetworkMessage) {
			t.Fatalf("%s failure=%#v stderr=%q", command, failure, stderr.String())
		}
	}
}

func TestMissingDeploymentDiagnostics(t *testing.T) {
	t.Setenv("BARN_HOME", t.TempDir())
	const missingMessage = "no deployment state found"
	for _, command := range []string{"logs", "ssh-config", "status", "ssh"} {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		if code := run([]string{"--json", command}, &stdout, &stderr); code != exitConflict {
			t.Fatalf("%s code=%d stdout=%q stderr=%q", command, code, stdout.String(), stderr.String())
		}
		var failure commandFailure
		if err := json.Unmarshal(stdout.Bytes(), &failure); err != nil {
			t.Fatalf("%s failure JSON: %v\n%s", command, err, stdout.String())
		}
		if failure.Error != "conflict" || failure.Message != missingMessage {
			t.Fatalf("%s failure=%#v stderr=%q", command, failure, stderr.String())
		}
	}
}

func TestHostsInstallDistinguishesMissingAndCorruptDeploymentState(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BARN_HOME", root)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run([]string{"--json", "hosts", "install"}, &stdout, &stderr); code != exitConflict {
		t.Fatalf("missing deployment code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var missing commandFailure
	if err := json.Unmarshal(stdout.Bytes(), &missing); err != nil || missing.Error != "conflict" || !strings.Contains(missing.Message, "no deployment state found") {
		t.Fatalf("missing deployment failure=%#v decode=%v", missing, err)
	}

	if err := os.WriteFile(filepath.Join(root, "state.json"), []byte("{\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--json", "hosts", "install"}, &stdout, &stderr); code != exitIntegrity {
		t.Fatalf("corrupt deployment code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var corrupt commandFailure
	if err := json.Unmarshal(stdout.Bytes(), &corrupt); err != nil || corrupt.Error != "integrity" || strings.Contains(corrupt.Message, "no deployment state found") {
		t.Fatalf("corrupt deployment failure=%#v decode=%v", corrupt, err)
	}
}

func TestSSHConfigRemoveRemainsAvailableAfterResolvedStateIsGone(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BARN_HOME", filepath.Join(root, "data"))
	home := filepath.Join(root, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	entry := sshconfig.Entry{
		Name: "lab", Node: "meta", User: "dba",
		HostKeyAlias: "barn-vm-test-instance",
		Host:         "127.0.0.1", Port: 2222, Identity: filepath.Join(root, "key"), KnownHosts: filepath.Join(root, "known"),
	}
	installed, err := sshconfig.InstallMany(home, []sshconfig.Entry{entry})
	if err != nil {
		t.Fatal(err)
	}
	// Destroy deliberately preserves keys and integration rollback authority;
	// removal must work with no deployment state present at all.
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	t.Setenv("HOME", home)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"ssh-config", "--remove", "--name", "lab", "--json"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("remove code=%d stderr=%s", code, stderr.String())
	}
	if _, err := os.Lstat(installed.Fragment); !os.IsNotExist(err) {
		t.Fatalf("owned fragment remains after state-independent remove: %v", err)
	}
}
