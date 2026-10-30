// Package portalloc implements deterministic host-port allocation.
package portalloc

import "fmt"

const candidateStep = 10000

// Candidates implements preferred+n*10000 until the uint16 port range ends.
// The first five values preserve Barn's historical choices; lower preferred
// ports get every remaining valid step instead of an arbitrary five-attempt
// ceiling.
func Candidates(preferred uint16) []uint16 {
	ports := make([]uint16, 0, 7)
	for candidate := uint32(preferred); candidate <= 65535; candidate += candidateStep {
		if candidate == 0 {
			break
		}
		ports = append(ports, uint16(candidate))
		if candidate > 65535-candidateStep {
			break
		}
	}
	return ports
}

// Choose returns the first candidate accepted by available. The caller owns
// the allocator lock and persistence; this function is deterministic and pure
// apart from the supplied availability probe.
func Choose(preferred uint16, available func(uint16) bool) (uint16, error) {
	if available == nil {
		return 0, fmt.Errorf("port availability probe is nil")
	}
	for _, port := range Candidates(preferred) {
		if available(port) {
			return port, nil
		}
	}
	return 0, fmt.Errorf("no loopback port available for preferred %d within the finite candidate set", preferred)
}
