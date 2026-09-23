package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/pgsty/farrow/internal/image"
	"github.com/pgsty/farrow/internal/naming"
	"github.com/pgsty/farrow/internal/network/subnet"
	"github.com/pgsty/farrow/internal/spec"
	"go.yaml.in/yaml/v3"
)

// The configuration is a Pigsty-compatible Ansible inventory. Farrow reads
// exactly the vm_* namespace plus a short whitelist of native Pigsty
// variables; everything else in the file is opaque and never validated.
//
// Strictness is inverted at the namespace boundary: unknown vm_* keys, wrong
// types, template expressions, and group-level conflicts are hard errors,
// while the surrounding Pigsty parameters are ignored entirely.

const maxInventoryBytes = 4 << 20

// InventoryDeploymentName is the fixed resolved-spec name for inventory-defined
// deployments. Naming from file content or directory would move the drift hash
// when the file or directory is renamed, so the name is deliberately constant;
// deployment identity is the single owner-scoped state root.
const InventoryDeploymentName = "farrow"

const (
	defaultCPU      = 2
	defaultMemMiB   = 4096
	defaultDiskGiB  = 64
	defaultDataGiB  = 128
	defaultSSHUser  = "dba"
	defaultAdminUID = 88
)

var defaultImage = image.EmbeddedCatalog().Defaults.Image

var knownVMKeys = map[string]struct{}{
	"vm_skip": {}, "vm_image": {}, "vm_version": {}, "vm_arch": {}, "vm_cpu": {}, "vm_mem": {},
	"vm_disk": {}, "vm_disks": {}, "vm_alias": {}, "vm_shares": {},
}

var derivedDiskName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// varSource is one contribution layer for a host: all.vars (depth 0), each
// group on the path to the host (depth = nesting level), and host vars
// (maximal depth). Deeper wins; two different values at the same depth are a
// conflict the user must resolve at host level.
type varSource struct {
	origin string
	depth  int
	vars   map[string]*yaml.Node
}

type inventoryHost struct {
	address string
	sources []varSource
}

func isMapping(node *yaml.Node) bool { return node != nil && node.Kind == yaml.MappingNode }

func resolveAlias(node *yaml.Node) (*yaml.Node, error) {
	seen := make(map[*yaml.Node]struct{})
	for depth := 0; node != nil && node.Kind == yaml.AliasNode; depth++ {
		if depth >= 32 {
			return nil, fmt.Errorf("YAML alias chain exceeds 32 links at line %d", node.Line)
		}
		if _, duplicate := seen[node]; duplicate || node.Alias == nil {
			return nil, fmt.Errorf("YAML alias cycle at line %d", node.Line)
		}
		seen[node] = struct{}{}
		node = node.Alias
	}
	return node, nil
}

func mappingEntries(node *yaml.Node) ([][2]*yaml.Node, error) {
	return effectiveMappingEntries(node, make(map[*yaml.Node]bool))
}

func isMergeKey(node *yaml.Node) bool {
	return node != nil && node.Kind == yaml.ScalarNode && node.Value == "<<" && (node.Tag == "" || node.Tag == "!!merge")
}

func effectiveMappingEntries(node *yaml.Node, visiting map[*yaml.Node]bool) ([][2]*yaml.Node, error) {
	var err error
	node, err = resolveAlias(node)
	if err != nil {
		return nil, err
	}
	if node == nil || node.Kind == 0 || node.Tag == "!!null" {
		return nil, nil
	}
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected a YAML mapping at line %d", node.Line)
	}
	if visiting[node] {
		return nil, fmt.Errorf("YAML mapping/alias cycle at line %d", node.Line)
	}
	visiting[node] = true
	defer delete(visiting, node)

	explicit := make([][2]*yaml.Node, 0, len(node.Content)/2)
	lines := make(map[string]int, len(node.Content)/2)
	var merge *yaml.Node
	mergeLine := 0
	for index := 0; index+1 < len(node.Content); index += 2 {
		key := node.Content[index]
		if key.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("YAML mapping key at line %d must be a scalar", key.Line)
		}
		if isMergeKey(key) {
			if merge != nil {
				return nil, fmt.Errorf("mapping at line %d repeats merge key << at line %d", node.Line, key.Line)
			}
			merge, mergeLine = node.Content[index+1], key.Line
			continue
		}
		if firstLine, duplicate := lines[key.Value]; duplicate {
			return nil, fmt.Errorf("duplicate key %q at line %d (already defined at line %d)", key.Value, key.Line, firstLine)
		}
		value, err := resolveAlias(node.Content[index+1])
		if err != nil {
			return nil, err
		}
		lines[key.Value] = key.Line
		explicit = append(explicit, [2]*yaml.Node{key, value})
	}
	entries := append([][2]*yaml.Node(nil), explicit...)
	if merge == nil {
		return entries, nil
	}
	merge, err = resolveAlias(merge)
	if err != nil {
		return nil, err
	}
	sources := []*yaml.Node{}
	switch {
	case merge != nil && merge.Kind == yaml.MappingNode:
		sources = append(sources, merge)
	case merge != nil && merge.Kind == yaml.SequenceNode:
		for _, item := range merge.Content {
			item, err = resolveAlias(item)
			if err != nil {
				return nil, err
			}
			if item == nil || item.Kind != yaml.MappingNode {
				return nil, fmt.Errorf("merge sequence item at line %d must be a mapping", mergeLine)
			}
			sources = append(sources, item)
		}
	default:
		return nil, fmt.Errorf("merge key at line %d must name a mapping or sequence of mappings", mergeLine)
	}
	// Explicit keys win over merged keys. In a merge sequence, the first source
	// wins over later sources, matching YAML/Ansible merge precedence.
	for _, source := range sources {
		merged, err := effectiveMappingEntries(source, visiting)
		if err != nil {
			return nil, err
		}
		for _, entry := range merged {
			if _, exists := lines[entry[0].Value]; exists {
				continue
			}
			lines[entry[0].Value] = entry[0].Line
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

func mappingLookup(node *yaml.Node, key string) (*yaml.Node, error) {
	entries, err := mappingEntries(node)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry[0].Value == key {
			return entry[1], nil
		}
	}
	return nil, nil
}

func varsOf(node *yaml.Node, origin string) (map[string]*yaml.Node, error) {
	entries, err := mappingEntries(node)
	if err != nil {
		return nil, fmt.Errorf("%s vars: %w", origin, err)
	}
	result := make(map[string]*yaml.Node, len(entries))
	for _, entry := range entries {
		key := entry[0].Value
		if strings.HasPrefix(key, "vm_") {
			if _, known := knownVMKeys[key]; !known {
				return nil, unknownVMKeyError(entry[0], origin)
			}
		}
		result[key] = entry[1]
	}
	return result, nil
}

// unknownVMKeyError keeps the vm_* namespace strict while pointing at the
// likely intended variable.
func unknownVMKeyError(key *yaml.Node, origin string) error {
	known := make([]string, 0, len(knownVMKeys))
	for name := range knownVMKeys {
		known = append(known, name)
	}
	sort.Strings(known)
	hint := "valid vm_* variables: " + strings.Join(known, ", ")
	if closest := naming.Closest(key.Value, known); closest != "" {
		hint = fmt.Sprintf("did you mean %s?", closest)
	}
	return fmt.Errorf("line %d: unknown variable %s in %s; %s", key.Line, key.Value, origin, hint)
}

// valueError locates a bad variable value: its line, the host it applies to,
// the value as written, and where it was inherited from when not set on the
// host itself.
func (host inventoryHost) valueError(node *yaml.Node, key, origin, rule string) error {
	value := "a " + map[yaml.Kind]string{yaml.SequenceNode: "list", yaml.MappingNode: "mapping"}[node.Kind]
	if node.Kind == yaml.ScalarNode {
		value = node.Value
		if node.Tag == "!!str" {
			value = strconv.Quote(node.Value)
		}
	}
	from := ""
	if !strings.HasSuffix(origin, " host "+host.address) {
		from = " (from " + origin + ")"
	}
	return fmt.Errorf("line %d: host %s %s = %s%s: %s", node.Line, host.address, key, value, from, rule)
}

func decodeAny(node *yaml.Node) (any, error) {
	var value any
	if err := node.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func containsTemplate(value any) bool {
	switch typed := value.(type) {
	case string:
		return strings.Contains(typed, "{{")
	case []any:
		for _, item := range typed {
			if containsTemplate(item) {
				return true
			}
		}
	case map[string]any:
		for _, item := range typed {
			if containsTemplate(item) {
				return true
			}
		}
	}
	return false
}

// lookup resolves one variable for a host across its contribution layers.
// It returns the winning YAML node, the origin it came from, and whether the
// key was found anywhere.
func (host inventoryHost) lookup(key string) (*yaml.Node, string, bool, error) {
	bestDepth := -1
	var winner *yaml.Node
	origin := ""
	var conflictOrigins []string
	for _, source := range host.sources {
		node, present := source.vars[key]
		if !present {
			continue
		}
		switch {
		case source.depth > bestDepth:
			bestDepth = source.depth
			winner = node
			origin = source.origin
			conflictOrigins = conflictOrigins[:0]
		case source.depth == bestDepth:
			equal := false
			if key == "vm_version" && winner.Kind == yaml.ScalarNode && node.Kind == yaml.ScalarNode {
				// YAML would decode both unquoted 9.10 and 9.1 to the same
				// float. Version selectors compare their preserved spelling.
				equal = strings.TrimSpace(winner.Value) == strings.TrimSpace(node.Value)
			} else {
				currentValue, currentErr := decodeAny(winner)
				candidateValue, candidateErr := decodeAny(node)
				equal = currentErr == nil && candidateErr == nil && reflect.DeepEqual(currentValue, candidateValue)
			}
			if !equal {
				conflictOrigins = append(conflictOrigins, source.origin)
			}
		}
	}
	if winner == nil {
		return nil, "", false, nil
	}
	if len(conflictOrigins) != 0 {
		return nil, "", false, fmt.Errorf("host %s inherits conflicting values for %q from %s and %s; set it at host level", host.address, key, origin, strings.Join(conflictOrigins, ", "))
	}
	value, err := decodeAny(winner)
	if err != nil {
		return nil, "", false, fmt.Errorf("host %s variable %q: %w", host.address, key, err)
	}
	if containsTemplate(value) {
		return nil, "", false, fmt.Errorf("host %s variable %q contains a template expression; farrow reads literal values only", host.address, key)
	}
	winner, err = resolveAlias(winner)
	return winner, origin, true, err
}

func (host inventoryHost) lookupString(key string) (string, bool, error) {
	node, origin, found, err := host.lookup(key)
	if err != nil || !found {
		return "", found, err
	}
	var value string
	if decodeErr := node.Decode(&value); decodeErr != nil {
		return "", true, host.valueError(node, key, origin, "must be a string")
	}
	return value, true, nil
}

// lookupVersionSelector preserves the scalar spelling so natural inventory
// forms such as vm_version: 9 and vm_version: 9.7 do not pass through floating
// point conversion. Quoted forms are accepted as well.
func (host inventoryHost) lookupVersionSelector(key string) (string, bool, error) {
	node, origin, found, err := host.lookup(key)
	if err != nil || !found {
		return "", found, err
	}
	if node.Kind != yaml.ScalarNode || (node.Tag != "!!str" && node.Tag != "!!int" && node.Tag != "!!float") {
		return "", true, host.valueError(node, key, origin, "must be a numeric version such as 9 or 9.7")
	}
	value := strings.TrimSpace(node.Value)
	if value == "" {
		return "", true, host.valueError(node, key, origin, "must not be empty")
	}
	return value, true, nil
}

// lookupInt reads an integer in [minimum, maximum].
func (host inventoryHost) lookupInt(key string, minimum, maximum int64) (int64, bool, error) {
	node, origin, found, err := host.lookup(key)
	if err != nil || !found {
		return 0, found, err
	}
	var value int64
	if decodeErr := node.Decode(&value); node.Tag != "!!int" || decodeErr != nil {
		return 0, true, host.valueError(node, key, origin, "must be an integer")
	}
	if value < minimum || value > maximum {
		return 0, true, host.valueError(node, key, origin, fmt.Sprintf("must be between %d and %d", minimum, maximum))
	}
	return value, true, nil
}

func (host inventoryHost) lookupBool(key string) (bool, bool, error) {
	node, origin, found, err := host.lookup(key)
	if err != nil || !found {
		return false, found, err
	}
	var value bool
	if decodeErr := node.Decode(&value); decodeErr != nil {
		return false, true, host.valueError(node, key, origin, "must be true or false")
	}
	return value, true, nil
}

// lookupSize accepts a bare integer scaled by unit (MiB for memory, GiB for
// disks) or a string with an explicit unit, and enforces a minimum.
func (host inventoryHost) lookupSize(key string, unit, minimum int64) (int64, bool, error) {
	node, origin, found, err := host.lookup(key)
	if err != nil || !found {
		return 0, found, err
	}
	unitName := "GiB"
	if unit == 1<<20 {
		unitName = "MiB"
	}
	invalid := func(rule string) error {
		return host.valueError(node, key, origin, fmt.Sprintf("%s (plain integers are %s; sizes such as 4GiB also work)", rule, unitName))
	}
	var value int64
	if node.Tag == "!!int" {
		var integer int64
		if err := node.Decode(&integer); err != nil {
			return 0, true, invalid("value is too large")
		}
		if value, err = scaleSize(integer, unit); err != nil {
			return 0, true, invalid(err.Error())
		}
	} else {
		var text string
		if decodeErr := node.Decode(&text); node.Tag != "!!str" || decodeErr != nil {
			return 0, true, invalid("must be an integer or a size")
		}
		if value, err = ParseSize(text); err != nil {
			return 0, true, invalid(err.Error())
		}
	}
	if value < minimum {
		return 0, true, invalid(fmt.Sprintf("must be at least %d %s", minimum/unit, unitName))
	}
	return value, true, nil
}

func (host inventoryHost) lookupStringList(key string) ([]string, bool, error) {
	node, origin, found, err := host.lookup(key)
	if err != nil || !found {
		return nil, found, err
	}
	var value []string
	if decodeErr := node.Decode(&value); decodeErr != nil {
		return nil, true, host.valueError(node, key, origin, "must be a list of strings")
	}
	return value, true, nil
}

func diskNameForPath(path string) (string, error) {
	name := strings.ReplaceAll(strings.Trim(path, "/"), "/", "-")
	if !derivedDiskName.MatchString(name) {
		return "", fmt.Errorf("disk mount %q does not derive a safe disk identity; use a short lowercase path such as /data", path)
	}
	return name, nil
}

func diskSizeBytes(host, path string, size any) (int64, error) {
	switch typed := size.(type) {
	case nil:
		return defaultDataGiB * spec.GiB, nil
	case int:
		if typed <= 0 {
			return 0, fmt.Errorf("host %s disk %s size must be positive", host, path)
		}
		return scaleSize(int64(typed), spec.GiB)
	case int64:
		if typed <= 0 {
			return 0, fmt.Errorf("host %s disk %s size must be positive", host, path)
		}
		return scaleSize(typed, spec.GiB)
	case string:
		value, err := ParseSize(typed)
		if err != nil {
			return 0, fmt.Errorf("host %s disk %s: %w", host, path, err)
		}
		return value, nil
	default:
		return 0, fmt.Errorf("host %s disk %s size must be an integer (GiB) or a size string", host, path)
	}
}

func (host inventoryHost) lookupDisks() ([]DiskConfig, bool, error) {
	node, origin, found, err := host.lookup("vm_disks")
	if err != nil || !found {
		return nil, found, err
	}
	var entries []map[string]any
	if decodeErr := node.Decode(&entries); decodeErr != nil {
		return nil, true, host.valueError(node, "vm_disks", origin, "must be a list of {path, size, fs, persistent} entries")
	}
	disks := make([]DiskConfig, 0, len(entries))
	for _, entry := range entries {
		for key := range entry {
			switch key {
			case "path", "size", "fs", "persistent":
			default:
				return nil, true, fmt.Errorf("host %s vm_disks entry has unknown key %q", host.address, key)
			}
		}
		path, _ := entry["path"].(string)
		if path == "" {
			return nil, true, fmt.Errorf("host %s vm_disks entry requires an absolute path", host.address)
		}
		name, nameErr := diskNameForPath(path)
		if nameErr != nil {
			return nil, true, fmt.Errorf("host %s: %w", host.address, nameErr)
		}
		size, sizeErr := diskSizeBytes(host.address, path, entry["size"])
		if sizeErr != nil {
			return nil, true, sizeErr
		}
		filesystem := "auto"
		if value, present := entry["fs"]; present {
			text, ok := value.(string)
			if !ok || !spec.ValidFilesystem(text) {
				return nil, true, fmt.Errorf("host %s disk %s fs must be auto, xfs, or ext4", host.address, path)
			}
			filesystem = text
		}
		persistent := false
		if value, present := entry["persistent"]; present {
			flag, ok := value.(bool)
			if !ok {
				return nil, true, fmt.Errorf("host %s disk %s persistent must be a boolean", host.address, path)
			}
			persistent = flag
		}
		disks = append(disks, DiskConfig{Name: name, Size: Size(size), Mount: path, Filesystem: filesystem, Persistent: persistent})
	}
	return disks, true, nil
}

func (host inventoryHost) lookupShares() ([]ShareConfig, bool, error) {
	node, origin, found, err := host.lookup("vm_shares")
	if err != nil || !found {
		return nil, found, err
	}
	var entries []map[string]any
	if decodeErr := node.Decode(&entries); decodeErr != nil {
		return nil, true, host.valueError(node, "vm_shares", origin, "must be a list of {host, guest, readonly} entries")
	}
	shares := make([]ShareConfig, 0, len(entries))
	for _, entry := range entries {
		for key := range entry {
			switch key {
			case "host", "guest", "readonly":
			default:
				return nil, true, fmt.Errorf("host %s vm_shares entry has unknown key %q", host.address, key)
			}
		}
		hostPath, _ := entry["host"].(string)
		guestPath, _ := entry["guest"].(string)
		if hostPath == "" || guestPath == "" {
			return nil, true, fmt.Errorf("host %s vm_shares entry requires host and guest paths", host.address)
		}
		share := ShareConfig{Host: hostPath, Guest: guestPath}
		if value, present := entry["readonly"]; present {
			flag, ok := value.(bool)
			if !ok {
				return nil, true, fmt.Errorf("host %s share %s readonly must be a boolean", host.address, guestPath)
			}
			share.Readonly = shareReadonlyYAML(flag)
		}
		shares = append(shares, share)
	}
	return shares, true, nil
}

// nodeName resolves the VM name: explicit nodename, then the Pigsty
// node_id_from_pg convention (<pg_cluster>-<pg_seq>), then node-<last octet>.
func (host inventoryHost) nodeName() (string, error) {
	if name, found, err := host.lookupString("nodename"); err != nil {
		return "", err
	} else if found && name != "" {
		return name, nil
	}
	cluster, clusterFound, clusterErr := host.lookupString("pg_cluster")
	if clusterErr != nil {
		return "", fmt.Errorf("%v (set nodename explicitly to name this VM)", clusterErr)
	}
	sequence, sequenceFound, sequenceErr := host.lookupInt("pg_seq", 0, 1<<31-1)
	if sequenceErr != nil {
		return "", fmt.Errorf("%v (set nodename explicitly to name this VM)", sequenceErr)
	}
	if clusterFound && sequenceFound && cluster != "" {
		name := fmt.Sprintf("%s-%d", cluster, sequence)
		if !naming.ValidNodeName(name) {
			return "", fmt.Errorf("host %s: node name %q derived from pg_cluster and pg_seq is invalid: %s; set nodename explicitly", host.address, name, naming.NodeNameRule)
		}
		return name, nil
	}
	address, err := netip.ParseAddr(host.address)
	if err != nil || !address.Is4() {
		return "", fmt.Errorf("inventory host key %q must be an IPv4 address", host.address)
	}
	octets := address.As4()
	return fmt.Sprintf("node-%d", octets[3]), nil
}

func collectHosts(group *yaml.Node, origin string, depth int, inherited []varSource, hosts *[]inventoryHost, index map[string]int, visiting map[*yaml.Node]bool) error {
	if depth > 64 {
		return fmt.Errorf("inventory group nesting exceeds 64 levels at %s", origin)
	}
	var err error
	group, err = resolveAlias(group)
	if err != nil {
		return err
	}
	if group == nil || group.Kind != yaml.MappingNode {
		return fmt.Errorf("%s must be a YAML mapping", origin)
	}
	if visiting[group] {
		return fmt.Errorf("inventory group/alias cycle at line %d", group.Line)
	}
	visiting[group] = true
	defer delete(visiting, group)
	varsNode, err := mappingLookup(group, "vars")
	if err != nil {
		return fmt.Errorf("%s: %w", origin, err)
	}
	groupVars, err := varsOf(varsNode, origin)
	if err != nil {
		return err
	}
	sources := inherited
	if len(groupVars) != 0 {
		sources = append(append([]varSource(nil), inherited...), varSource{origin: origin, depth: depth, vars: groupVars})
	}
	hostsNode, err := mappingLookup(group, "hosts")
	if err != nil {
		return fmt.Errorf("%s: %w", origin, err)
	}
	hostEntries, err := mappingEntries(hostsNode)
	if err != nil {
		return fmt.Errorf("%s hosts: %w", origin, err)
	}
	for _, entry := range hostEntries {
		address := entry[0].Value
		if parsed, parseErr := netip.ParseAddr(address); parseErr != nil || !parsed.Is4() {
			return fmt.Errorf("%s host key %q must be an IPv4 address", origin, address)
		}
		hostVars, err := varsOf(entry[1], origin+" host "+address)
		if err != nil {
			return err
		}
		position, exists := index[address]
		if !exists {
			*hosts = append(*hosts, inventoryHost{address: address})
			position = len(*hosts) - 1
			index[address] = position
		}
		host := &(*hosts)[position]
		host.sources = append(host.sources, sources...)
		if len(hostVars) != 0 {
			host.sources = append(host.sources, varSource{origin: origin + " host " + address, depth: 1 << 20, vars: hostVars})
		}
	}
	childrenNode, err := mappingLookup(group, "children")
	if err != nil {
		return fmt.Errorf("%s: %w", origin, err)
	}
	childEntries, err := mappingEntries(childrenNode)
	if err != nil {
		return fmt.Errorf("%s children: %w", origin, err)
	}
	for _, child := range childEntries {
		if err := collectHosts(child[1], "group "+child[0].Value, depth+1, sources, hosts, index, visiting); err != nil {
			return err
		}
	}
	return nil
}

func dedupeVarSources(host inventoryHost) inventoryHost {
	seen := make(map[string]struct{}, len(host.sources))
	deduped := make([]varSource, 0, len(host.sources))
	for _, source := range host.sources {
		key := fmt.Sprintf("%s\x00%d", source.origin, source.depth)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		deduped = append(deduped, source)
	}
	host.sources = deduped
	return host
}

func inventoryCIDR(hosts []inventoryHost) (subnet.Layout, error) {
	prefixes := make(map[string]string) // prefix -> first host in it
	for _, host := range hosts {
		address, err := netip.ParseAddr(host.address)
		if err != nil || !address.Is4() {
			return subnet.Layout{}, fmt.Errorf("inventory host key %q must be an IPv4 address", host.address)
		}
		octets := address.As4()
		prefix := fmt.Sprintf("%d.%d.%d.0/24", octets[0], octets[1], octets[2])
		if _, seen := prefixes[prefix]; !seen {
			prefixes[prefix] = host.address
		}
	}
	if len(prefixes) != 1 {
		list := make([]string, 0, len(prefixes))
		for prefix := range prefixes {
			list = append(list, prefix)
		}
		sort.Strings(list)
		return subnet.Layout{}, fmt.Errorf("all managed hosts must share one /24; inventory spans %s", strings.Join(list, ", "))
	}
	for prefix, host := range prefixes {
		layout, err := subnet.Parse(prefix)
		if err != nil {
			return subnet.Layout{}, fmt.Errorf("host %s: %w", host, err)
		}
		return layout, nil
	}
	return subnet.Layout{}, errors.New("inventory has no managed hosts")
}

var yamlLinePrefix = regexp.MustCompile(`^yaml: line (\d+): `)

// inventoryDocument decodes exactly one YAML document, phrasing decoder errors
// for people: "invalid YAML at line 3: …" rather than "yaml: line 3: …".
func inventoryDocument(data []byte) (yaml.Node, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document, extra yaml.Node
	if err := decoder.Decode(&document); err != nil {
		if errors.Is(err, io.EOF) {
			return document, errors.New("inventory is empty")
		}
		message := strings.TrimPrefix(yamlLinePrefix.ReplaceAllString(err.Error(), "invalid YAML at line $1: "), "yaml: ")
		if !strings.HasPrefix(message, "invalid YAML") {
			message = "invalid YAML: " + message
		}
		return document, errors.New(message)
	}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return document, err
		}
		return document, errors.New("inventory must be exactly one YAML document")
	}
	return document, nil
}

// ParseInventory reads a Pigsty-compatible inventory document and returns the
// equivalent internal configuration.
func ParseInventory(data []byte) (File, error) {
	if len(data) > maxInventoryBytes {
		return File{}, errors.New("inventory exceeds the 4 MiB limit")
	}
	document, err := inventoryDocument(data)
	if err != nil {
		return File{}, err
	}
	if len(document.Content) != 1 {
		return File{}, errors.New("inventory must be exactly one YAML document")
	}
	root := document.Content[0]
	if !isMapping(root) {
		return File{}, errors.New("inventory root must be a mapping with an all: group")
	}
	all, err := mappingLookup(root, "all")
	if err != nil {
		return File{}, err
	}
	if all == nil {
		return File{}, errors.New("inventory has no all: group")
	}
	globalVarsNode, err := mappingLookup(all, "vars")
	if err != nil {
		return File{}, fmt.Errorf("all: %w", err)
	}
	globalVars, err := varsOf(globalVarsNode, "all.vars")
	if err != nil {
		return File{}, err
	}
	hosts := make([]inventoryHost, 0)
	index := make(map[string]int)
	inherited := []varSource{}
	if len(globalVars) != 0 {
		inherited = append(inherited, varSource{origin: "all.vars", depth: 0, vars: globalVars})
	}
	if err := collectHosts(all, "all", 0, inherited, &hosts, index, make(map[*yaml.Node]bool)); err != nil {
		return File{}, err
	}
	if len(hosts) == 0 {
		return File{}, errors.New("inventory defines no hosts")
	}

	adminIP := ""
	if node, present := globalVars["admin_ip"]; present {
		if err := node.Decode(&adminIP); err != nil {
			return File{}, errors.New("all.vars admin_ip must be a string")
		}
	}

	managed := make([]inventoryHost, 0, len(hosts))
	for _, host := range hosts {
		host = dedupeVarSources(host)
		skip, _, err := host.lookupBool("vm_skip")
		if err != nil {
			return File{}, err
		}
		if skip {
			continue
		}
		managed = append(managed, host)
	}
	if len(managed) == 0 {
		return File{}, errors.New("inventory has no managed hosts; every host sets vm_skip: true")
	}
	layout, err := inventoryCIDR(managed)
	if err != nil {
		return File{}, err
	}

	file := File{
		Version: 1,
		Name:    InventoryDeploymentName,
		Arch:    "native",
		Network: NetworkConfig{Mode: "private", CIDR: layout.CIDR(), HostAddress: layout.HostAddress(), DHCPEnd: layout.DHCPEnd()},
	}

	sshUser := ""
	sshUserOwner := ""
	vmArch := ""
	vmArchOwner := ""
	vmArchHosts := 0
	controlIndex := -1
	for _, host := range managed {
		name, err := host.nodeName()
		if err != nil {
			return File{}, err
		}
		node := NodeConfig{Name: name, Address: host.address}

		imageAlias := defaultImage
		if value, found, err := host.lookupString("vm_image"); err != nil {
			return File{}, err
		} else if found {
			imageAlias = value
		}
		versionSelector, versionFound, err := host.lookupVersionSelector("vm_version")
		if err != nil {
			return File{}, err
		}
		if versionFound {
			node.Image, err = image.CanonicalVersionReference(imageAlias, versionSelector)
		} else {
			node.Image, err = image.CanonicalReference(imageAlias)
		}
		if err != nil {
			return File{}, fmt.Errorf("host %s vm_image/vm_version: %w", host.address, err)
		}

		if value, found, err := host.lookupString("vm_arch"); err != nil {
			return File{}, err
		} else if found {
			value = strings.ToLower(strings.TrimSpace(value))
			if value != "native" && value != "amd64" && value != "arm64" {
				return File{}, fmt.Errorf("host %s vm_arch must be native, amd64, or arm64", host.address)
			}
			vmArchHosts++
			if vmArchHosts == 1 {
				vmArch, vmArchOwner = value, host.address
			} else if vmArch != value {
				return File{}, fmt.Errorf("hosts %s and %s declare different vm_arch values; farrow uses one guest architecture per deployment", vmArchOwner, host.address)
			}
		}

		node.CPUs = defaultCPU
		if value, found, err := host.lookupInt("vm_cpu", 1, maxVirtualCPUs); err != nil {
			return File{}, err
		} else if found {
			node.CPUs = int(value)
		}

		node.Memory = Size(defaultMemMiB << 20)
		if value, found, err := host.lookupSize("vm_mem", 1<<20, minMemory); err != nil {
			return File{}, err
		} else if found {
			node.Memory = Size(value)
		}

		node.RootDisk = Size(defaultDiskGiB * spec.GiB)
		if value, found, err := host.lookupSize("vm_disk", spec.GiB, 0); err != nil {
			return File{}, err
		} else if found {
			node.RootDisk = Size(value)
		}

		if disks, found, err := host.lookupDisks(); err != nil {
			return File{}, err
		} else if found {
			node.Disks = disks
		} else {
			node.Disks = []DiskConfig{{Name: "data", Size: Size(defaultDataGiB * spec.GiB), Mount: "/data", Filesystem: "auto"}}
		}

		if aliases, found, err := host.lookupStringList("vm_alias"); err != nil {
			return File{}, err
		} else if found {
			node.HostAliases = aliases
		}

		if shares, found, err := host.lookupShares(); err != nil {
			return File{}, err
		} else if found {
			node.Shares = shares
		}

		user := defaultSSHUser
		if value, found, err := host.lookupString("node_admin_username"); err != nil {
			return File{}, err
		} else if found && value != "" {
			user = value
		}
		if sshUser == "" {
			sshUser, sshUserOwner = user, host.address
		} else if sshUser != user {
			return File{}, fmt.Errorf("hosts %s and %s declare different node_admin_username values; farrow uses one login user per deployment", sshUserOwner, host.address)
		}
		if value, found, err := host.lookupInt("node_admin_uid", 0, 1<<31-1); err != nil {
			return File{}, err
		} else if found && user == defaultSSHUser && value != defaultAdminUID {
			return File{}, fmt.Errorf("host %s sets node_admin_uid=%d; farrow provisions %s with the fixed UID %d", host.address, value, defaultSSHUser, defaultAdminUID)
		}

		if host.address == adminIP {
			controlIndex = len(file.Nodes)
		}
		file.Nodes = append(file.Nodes, node)
	}
	if controlIndex < 0 {
		controlIndex = 0
	}
	file.Nodes[controlIndex].Control = true
	file.SSH.User = sshUser
	if vmArchHosts != 0 {
		if vmArchHosts != len(managed) {
			return File{}, errors.New("vm_arch must resolve on every managed host; define it once in all.vars")
		}
		file.Arch = vmArch
	}

	if err := file.Validate(); err != nil {
		return File{}, err
	}
	return file, nil
}

// DetectFormat classifies raw configuration bytes without fully parsing them.
func DetectFormat(data []byte) (string, error) {
	document, err := inventoryDocument(data)
	if err != nil {
		return "", err
	}
	if len(document.Content) == 0 {
		return "", errors.New("inventory is empty")
	}
	root := document.Content[0]
	if !isMapping(root) {
		return "", errors.New("configuration root must be a YAML mapping")
	}
	all, err := mappingLookup(root, "all")
	if err != nil {
		return "", err
	}
	if all != nil {
		return "inventory", nil
	}
	return "", errors.New("unrecognized configuration: expected a Pigsty-compatible inventory with a top-level all: group")
}
