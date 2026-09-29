package image

import (
	"errors"
	"fmt"
	"strings"
)

// Reference is stable user intent. Channels and numeric version prefixes are
// movable here; resolving either to an immutable release is deliberately a
// catalog operation and never changes the deployment spec hash.
type Reference struct {
	Image   string `json:"image"`
	Channel string `json:"channel,omitempty"`
	Version string `json:"version,omitempty"`
}

// ParseReference accepts image, image:channel, or image@version-selector.
// Architecture stays orthogonal because Barn already models it as
// deployment-wide vm_arch.
func ParseReference(value string) (Reference, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return Reference{}, nil
	}
	if strings.ContainsAny(value, "\x00\r\n/\\") {
		return Reference{}, errors.New("image reference contains an unsafe path or control character")
	}
	if strings.Count(value, "@") > 1 || strings.Count(value, ":") > 1 || (strings.Contains(value, "@") && strings.Contains(value, ":")) {
		return Reference{}, errors.New("image reference must use at most one channel (:name) or version selector (@version)")
	}
	ref := Reference{}
	switch {
	case strings.Contains(value, "@"):
		ref.Image, ref.Version, _ = strings.Cut(value, "@")
		if strings.TrimSpace(ref.Version) == "" {
			return Reference{}, errors.New("image version after @ must not be empty")
		}
	case strings.Contains(value, ":"):
		ref.Image, ref.Channel, _ = strings.Cut(value, ":")
		if strings.TrimSpace(ref.Channel) == "" {
			return Reference{}, errors.New("image channel after : must not be empty")
		}
	default:
		ref.Image = value
	}
	ref.Image = strings.ToLower(strings.TrimSpace(ref.Image))
	ref.Channel = strings.ToLower(strings.TrimSpace(ref.Channel))
	ref.Version = strings.TrimSpace(ref.Version)
	if ref.Image == "" || !catalogName.MatchString(ref.Image) {
		return Reference{}, fmt.Errorf("invalid image name %q", ref.Image)
	}
	if ref.Channel != "" && !channelName.MatchString(ref.Channel) {
		return Reference{}, fmt.Errorf("invalid image channel %q", ref.Channel)
	}
	if ref.Version != "" && !releaseName.MatchString(ref.Version) {
		return Reference{}, fmt.Errorf("invalid image version %q", ref.Version)
	}
	return ref, nil
}

// CanonicalVersionReference combines a bare image name with a numeric version
// selector such as 9 or 9.7. Catalog resolution later chooses the numerically
// newest release on that dot-component prefix.
func CanonicalVersionReference(value, selector string) (string, error) {
	ref, err := ParseReference(value)
	if err != nil {
		return "", err
	}
	if ref.Channel != "" || ref.Version != "" {
		return "", errors.New("a separate version selector requires a bare image without :channel or @version")
	}
	selector = strings.TrimSpace(selector)
	if _, err := numericVersionParts(selector); err != nil {
		return "", err
	}
	ref.Image = CanonicalAlias(ref.Image)
	ref.Version = selector
	return ref.String(), nil
}

func (r Reference) String() string {
	if r.Image == "" {
		return ""
	}
	if r.Version != "" {
		return r.Image + "@" + r.Version
	}
	if r.Channel != "" {
		return r.Image + ":" + r.Channel
	}
	return r.Image
}
