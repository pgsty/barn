// Package naming owns identifiers shared across Farrow's filesystem, state,
// QEMU, provisioning, and guest-rendering boundaries.
package naming

import "regexp"

var nodeNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidNodeName reports whether name is a safe DNS label and path component.
func ValidNodeName(name string) bool {
	return nodeNamePattern.MatchString(name)
}

// Closest returns the candidate within two edits of name, for "did you mean"
// hints. It returns "" when nothing is close or name itself is a candidate.
func Closest(name string, candidates []string) string {
	best, bestDistance := "", 3
	for _, candidate := range candidates {
		if candidate == name {
			return ""
		}
		if distance := editDistance(name, candidate); distance < bestDistance {
			best, bestDistance = candidate, distance
		}
	}
	return best
}

func editDistance(a, b string) int {
	previous := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current := make([]int, len(b)+1)
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			current[j] = min(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
		}
		previous = current
	}
	return previous[len(b)]
}
