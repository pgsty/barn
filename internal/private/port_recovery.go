package private

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/pgsty/barn/internal/network/portalloc"
	"github.com/pgsty/barn/internal/state"
)

func (NativeLifecycle) PortAvailable(bind string, port uint16) bool {
	listener, err := net.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(int(port))))
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}

func reservedNodePorts(store state.Store) (map[uint16]struct{}, error) {
	deployment, err := store.ReadDeployment()
	if err != nil {
		return nil, err
	}
	reserved := make(map[uint16]struct{})
	for _, definition := range deployment.Resolved.Nodes {
		node, err := store.ReadNode(definition.Name)
		if missingPath(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		reserved[node.SSHPort] = struct{}{}
		for _, forward := range node.Forwards {
			reserved[forward.Host] = struct{}{}
		}
	}
	return reserved, nil
}

// Only the automatically allocated management forward may move. The caller
// holds the deployment lock and persists the returned node before launching
// QEMU, so its argv, connection details and process identity agree.
func repairManagementPort(node state.NodeState, reserved map[uint16]struct{}, available func(string, uint16) bool) (state.NodeState, []string, error) {
	if node.Phase == state.Running || available("127.0.0.1", node.SSHPort) {
		return node, nil, nil
	}
	if len(node.Forwards) == 0 || node.Forwards[0].Bind != "127.0.0.1" || node.Forwards[0].Guest != 22 || node.Forwards[0].Host != node.SSHPort {
		return node, nil, fmt.Errorf("cannot identify management SSH forward for %s", node.Node)
	}
	// Start from the original candidate family, even after earlier repairs.
	preferred := node.SSHPort % 10000
	if preferred == 0 {
		preferred = 10000
	}
	port, err := portalloc.Choose(preferred, func(candidate uint16) bool {
		_, used := reserved[candidate]
		return !used && available("127.0.0.1", candidate)
	})
	if err != nil {
		return node, nil, fmt.Errorf("recover management SSH port for %s: %w", node.Node, err)
	}
	oldRule := fmt.Sprintf("hostfwd=tcp:127.0.0.1:%d-:22", node.SSHPort)
	newRule := fmt.Sprintf("hostfwd=tcp:127.0.0.1:%d-:22", port)
	args := append([]string(nil), node.Invocation.Args...)
	replacements := 0
	for index := 0; index+1 < len(args); index++ {
		if args[index] != "-netdev" || !strings.HasPrefix(args[index+1], "user,id=mgmt,") {
			continue
		}
		options := strings.Split(args[index+1], ",")
		for option, value := range options {
			if value == oldRule {
				options[option] = newRule
				replacements++
			}
		}
		args[index+1] = strings.Join(options, ",")
	}
	if replacements != 1 {
		return node, nil, fmt.Errorf("cannot identify management SSH argv for %s", node.Node)
	}
	repairs := []string{fmt.Sprintf("SSH port %d busy; using %d", node.SSHPort, port)}
	node.Invocation.Args = args
	node.Forwards = append(node.Forwards[:0:0], node.Forwards...)
	node.Forwards[0].Host = port
	node.SSHPort = port
	reserved[port] = struct{}{}
	return node, repairs, nil
}
