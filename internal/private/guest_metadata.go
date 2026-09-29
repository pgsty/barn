package private

import (
	"context"
	"encoding/base64"

	"github.com/pgsty/barn/internal/cloudinit"
	"github.com/pgsty/barn/internal/lock"
	"github.com/pgsty/barn/internal/spec"
	"github.com/pgsty/barn/internal/state"
	"github.com/pgsty/barn/internal/vm"
)

func guestMetadataCommand(resolved spec.Resolved, control bool) string {
	hosts := deploymentHosts(resolved)
	write := func(path, contents string) string {
		return "printf %s " + base64.StdEncoding.EncodeToString([]byte(contents)) + " | base64 -d | sudo -n tee " + path + " >/dev/null"
	}
	command := "set -eu; " + write("/usr/local/libexec/barn-hosts", cloudinit.RenderHostsScript(hosts)) + " && sudo -n /usr/local/libexec/barn-hosts"
	if control {
		payload := base64.StdEncoding.EncodeToString([]byte(cloudinit.RenderControlSSHConfig(resolved.SSHUser, hosts)))
		command += ` && temporary=$(mktemp "$HOME/.ssh/.barn-XXXXXX") && { printf %s ` + payload + ` | base64 -d; if test -f "$HOME/.ssh/config"; then sed '/^# BEGIN BARN$/,/^# END BARN$/d' "$HOME/.ssh/config"; fi; } >"$temporary" && install -m 600 "$temporary" "$HOME/.ssh/config" && rm -f "$temporary"`
	}
	return command
}

// RefreshGuestMetadata updates the small Barn-owned hosts/SSH fragments in
// running guests after topology changes. Stopped guests catch up on their next up.
func (m Manager) RefreshGuestMetadata(ctx context.Context) (returnErr error) {
	deployment, err := m.openDeployment(false)
	if err != nil {
		return err
	}
	held, err := acquireDeploymentLock(ctx, deployment.Root, false, m.Progress)
	if err != nil {
		return err
	}
	defer func() { returnErr = lock.JoinRelease(returnErr, held, "guest metadata lock") }()
	store := state.Store{Root: deployment.Root}
	current, err := store.ReadDeployment()
	if err != nil {
		return err
	}
	sshPath, err := m.lookPath("ssh")
	if err != nil {
		return err
	}
	failures := make([]NodeFailure, 0)
	attempted := 0
	for _, definition := range current.Resolved.Nodes {
		node, err := store.ReadNode(definition.Name)
		if missingPath(err) {
			continue
		}
		if err != nil {
			return err
		}
		if node.Phase != state.Running {
			continue
		}
		attempted++
		selected := m
		selected.Nodes = []string{node.Node}
		connections, err := selected.ConnectionsLocked(ctx, deployment, held)
		if err == nil {
			connection := connections[0]
			args := vm.SSHArgsForInstance(connection.User, connection.PrivateKey, connection.KnownHosts, connection.HostKeyAlias, connection.Port)
			args = append(args, guestMetadataCommand(current.Resolved, definition.Control))
			_, err = m.runner().Run(ctx, sshPath, args...)
		}
		if err != nil {
			failures = append(failures, NodeFailure{Node: node.Node, Stage: "guest-metadata", Error: err.Error()})
		}
	}
	if len(failures) != 0 {
		return newPartialError(failures, attempted)
	}
	return nil
}
