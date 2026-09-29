package doctor

import "github.com/pgsty/barn/internal/spec"

func quickResolved(withDataDisk, withDefaultForwards bool) spec.Resolved {
	node := spec.Node{Name: "meta", Control: true, Address: "10.10.10.10", CPUs: 2, Memory: 4 * spec.GiB, RootDisk: 64 * spec.GiB}
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
	return spec.Resolved{Schema: 1, Name: "quick", Image: "u24", Network: "private", Private: &spec.PrivateNetwork{CIDR: "10.10.10.0/24", HostAddress: "10.10.10.1", DHCPEnd: "10.10.10.8"}, SSHUser: "dba", SSHWaitTimeoutNS: int64(spec.DefaultSSHWaitTimeout), Nodes: []spec.Node{node}}
}
