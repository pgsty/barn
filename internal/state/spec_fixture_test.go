package state

import "github.com/pgsty/barn/internal/spec"

func quickResolved(withDataDisk, withDefaultForwards bool) spec.Resolved {
	node := spec.Node{Name: "meta", Control: true, CPUs: 2, Memory: 4 * spec.GiB, RootDisk: 64 * spec.GiB}
	if withDataDisk {
		node.Disks = []spec.Disk{{Name: "data", Size: 64 * spec.GiB, Mount: "/data"}}
	}
	if withDefaultForwards {
		node.Forwards = []spec.Forward{
			{Bind: "127.0.0.1", Host: 15432, Guest: 5432, Protocol: "tcp"},
			{Bind: "127.0.0.1", Host: 13000, Guest: 3000, Protocol: "tcp"},
			{Bind: "127.0.0.1", Host: 18080, Guest: 80, Protocol: "tcp"},
			{Bind: "127.0.0.1", Host: 18443, Guest: 443, Protocol: "tcp"},
		}
	}
	// Keep the retired user network so persisted legacy-state fixtures stay exact.
	return spec.Resolved{Schema: 1, Name: "quick", Image: "u24", Network: "user", SSHUser: "dba", SSHWaitTimeoutNS: int64(spec.DefaultSSHWaitTimeout), Nodes: []spec.Node{node}}
}
