package state

import "errors"

// RootResize survives both process restarts and metadata write failures. Its
// target is absolute: an interrupted growth is never undone by shrinking.
type RootResize struct {
	Original   int64          `json:"original"`
	Target     int64          `json:"target"`
	WasRunning bool           `json:"was_running"`
	Stage      string         `json:"stage"`
	Guest      RootFilesystem `json:"guest"`
}

type RootFilesystem struct {
	Serial     string `json:"serial"`
	Partition  int    `json:"partition"`
	Start      int64  `json:"start"`
	UUID       string `json:"uuid"`
	PartUUID   string `json:"part_uuid"`
	Filesystem string `json:"filesystem"`
	DiskBytes  int64  `json:"disk_bytes"`
	PartBytes  int64  `json:"part_bytes"`
	FSBytes    int64  `json:"fs_bytes"`
}

func (r RootResize) Validate() error {
	if r.Original <= 0 || r.Target <= r.Original || r.Target%512 != 0 {
		return errors.New("root resize journal has invalid sizes")
	}
	switch r.Stage {
	case "preflight":
		return nil
	case "image", "guest", "verified":
		g := r.Guest
		if g.Serial != "" && g.Partition > 0 && g.Start > 0 && g.UUID != "" && g.PartUUID != "" && (g.Filesystem == "ext4" || g.Filesystem == "xfs") && g.DiskBytes == r.Original && g.PartBytes > 0 && g.FSBytes > 0 && g.FSBytes <= g.PartBytes && g.Start < g.DiskBytes && g.PartBytes <= g.DiskBytes-g.Start {
			return nil
		}
	}
	return errors.New("root resize journal has an invalid stage or guest identity")
}
