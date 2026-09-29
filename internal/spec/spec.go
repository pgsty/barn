// Package spec contains Barn's typed, resolved inventory model.
package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

const GiB int64 = 1024 * 1024 * 1024

type Forward struct {
	Bind          string `json:"bind"`
	Host          uint16 `json:"host"`
	RequestedHost uint16 `json:"requested_host,omitempty"`
	Guest         uint16 `json:"guest"`
	Protocol      string `json:"protocol"`
}

// RequestedHostPort returns the user's preferred host port. RequestedHost is
// set only when allocation chose a different port; otherwise Host is the request.
func RequestedHostPort(forward Forward) uint16 {
	if forward.RequestedHost != 0 {
		return forward.RequestedHost
	}
	return forward.Host
}

// WithMaterializedHost records the original request only when allocation had
// to choose a different port. Keeping the field optional preserves the
// canonical JSON and hash of deployments whose preferred port was available.
func WithMaterializedHost(forward Forward, host uint16) Forward {
	requested := RequestedHostPort(forward)
	forward.Host = host
	if host == requested {
		forward.RequestedHost = 0
	} else {
		forward.RequestedHost = requested
	}
	return forward
}

// ReuseMaterializedForwardPorts carries forward committed allocation choices
// only when both the route identity and the original request still match. Each
// persisted entry may satisfy at most one desired entry so duplicate routes
// are paired one-to-one instead of all reusing the first match.
func ReuseMaterializedForwardPorts(desired, persisted []Forward) []Forward {
	result := append([]Forward(nil), desired...)
	consumed := make([]bool, len(persisted))
	for desiredIndex := range result {
		candidate := result[desiredIndex]
		requested := RequestedHostPort(candidate)
		for persistedIndex, current := range persisted {
			if consumed[persistedIndex] || candidate.Bind != current.Bind || candidate.Guest != current.Guest || candidate.Protocol != current.Protocol || requested != RequestedHostPort(current) {
				continue
			}
			result[desiredIndex].Host = current.Host
			result[desiredIndex].RequestedHost = current.RequestedHost
			consumed[persistedIndex] = true
			break
		}
	}
	return result
}

type Disk struct {
	Name       string `json:"name"`
	Size       int64  `json:"size_bytes"`
	Mount      string `json:"mount"`
	Filesystem string `json:"filesystem,omitempty"`
	Persistent bool   `json:"persistent"`
}

// ValidFilesystem accepts the data-disk filesystem requests: xfs, ext4, or
// auto (XFS when the guest has mkfs.xfs, otherwise ext4).
func ValidFilesystem(value string) bool {
	return value == "auto" || value == "xfs" || value == "ext4"
}

// ValidFormattedFilesystem accepts what a formatted disk can actually be.
func ValidFormattedFilesystem(value string) bool {
	return value == "xfs" || value == "ext4"
}

const MaxSharesPerNode = 8

type Share struct {
	Host     string `json:"host"`
	Guest    string `json:"guest"`
	Readonly bool   `json:"readonly"`
}

// ShareTag returns the stable virtio-9p mount tag for one resolved share.
// The separators make the digest unambiguous without exposing either path in
// QEMU device identifiers. Twenty hexadecimal digits keep the complete tag
// below QEMU's 31-byte mount-tag limit.
func ShareTag(share Share) string {
	digest := sha256.Sum256([]byte(share.Host + "\x00" + share.Guest + "\x00" + strconv.FormatBool(share.Readonly)))
	return "barn-" + hex.EncodeToString(digest[:])[:20]
}

type Node struct {
	Name     string    `json:"name"`
	Control  bool      `json:"control"`
	Address  string    `json:"address,omitempty"`
	Image    string    `json:"image,omitempty"`
	Aliases  []string  `json:"host_aliases,omitempty"`
	CPUs     int       `json:"cpus"`
	Memory   int64     `json:"memory_bytes"`
	RootDisk int64     `json:"root_disk_bytes"`
	Disks    []Disk    `json:"disks,omitempty"`
	Forwards []Forward `json:"forwards,omitempty"`
	Shares   []Share   `json:"shares,omitempty"`
}

type PrivateNetwork struct {
	CIDR        string `json:"cidr"`
	HostAddress string `json:"host_address"`
	DHCPEnd     string `json:"dhcp_end"`
}

type Resolved struct {
	Schema           int             `json:"schema"`
	Name             string          `json:"name"`
	Arch             string          `json:"arch,omitempty"`
	Image            string          `json:"image"`
	Network          string          `json:"network"`
	Private          *PrivateNetwork `json:"private,omitempty"`
	SSHUser          string          `json:"ssh_user"`
	SSHWaitTimeoutNS int64           `json:"ssh_wait_timeout_ns,omitempty"`
	Nodes            []Node          `json:"nodes"`
}

const DefaultSSHWaitTimeout = 180 * time.Second

// SSHWaitTimeout applies the 180-second default when unset. Negative values
// fail closed.
func (r Resolved) SSHWaitTimeout() (time.Duration, error) {
	if r.SSHWaitTimeoutNS == 0 {
		return DefaultSSHWaitTimeout, nil
	}
	if r.SSHWaitTimeoutNS < 0 {
		return 0, errors.New("resolved SSH wait timeout must be positive")
	}
	return time.Duration(r.SSHWaitTimeoutNS), nil
}

// CanonicalJSON relies only on structs and slices; there are no unordered map
// keys in the resolved model.
func CanonicalJSON(value Resolved) ([]byte, error) { return json.Marshal(value) }

func Hash(value Resolved) (string, error) {
	data, err := CanonicalJSON(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

// NodeResolved narrows a resolved spec to its deployment envelope plus exactly
// one node. Its hash is the node-scoped drift identity: adding or changing a
// peer never moves another node's hash, while any envelope change (network,
// image default, SSH user) moves every node. A single-node deployment's node
// hash equals its deployment hash by construction.
func NodeResolved(value Resolved, name string) (Resolved, bool) {
	for _, node := range value.Nodes {
		if node.Name == name {
			narrowed := value
			if value.Private != nil {
				privateNetwork := *value.Private
				narrowed.Private = &privateNetwork
			}
			narrowed.Nodes = []Node{node}
			return narrowed, true
		}
	}
	return Resolved{}, false
}

func NodeHash(value Resolved, name string) (string, error) {
	narrowed, ok := NodeResolved(value, name)
	if !ok {
		return "", errors.New("resolved spec has no node " + name)
	}
	return Hash(narrowed)
}

func NodeHashes(value Resolved) (map[string]string, error) {
	result := make(map[string]string, len(value.Nodes))
	for _, node := range value.Nodes {
		hash, err := NodeHash(value, node.Name)
		if err != nil {
			return nil, err
		}
		result[node.Name] = hash
	}
	return result, nil
}
