// Package naming owns identifiers shared across Farrow's filesystem, state,
// QEMU, provisioning, and guest-rendering boundaries.
package naming

import "regexp"

var nodeNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidNodeName reports whether name is a safe DNS label and path component.
func ValidNodeName(name string) bool {
	return nodeNamePattern.MatchString(name)
}

// NodeNameRule states the node-name contract for error messages.
const NodeNameRule = "use lowercase letters, digits and '-', at most 63 characters, not starting or ending with '-'"

// Closest returns the candidate within two single-character edits of name
// (one for names of four characters or fewer, where two edits reach almost
// anything), for "did you mean" hints. It returns "" when name matches exactly
// or nothing is close enough.
func Closest(name string, candidates []string) string {
	best, bestDistance := "", 3
	if len(name) <= 4 {
		bestDistance = 2
	}
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

func editDistance(first, second string) int {
	previous := make([]int, len(second)+1)
	for index := range previous {
		previous[index] = index
	}
	for i := 1; i <= len(first); i++ {
		current := make([]int, len(second)+1)
		current[0] = i
		for j := 1; j <= len(second); j++ {
			cost := 1
			if first[i-1] == second[j-1] {
				cost = 0
			}
			current[j] = min(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
		}
		previous = current
	}
	return previous[len(second)]
}
