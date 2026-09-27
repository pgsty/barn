package macvm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

type NetworkObservedSource struct {
	IP       string `json:"ip"`
	MAC      string `json:"mac"`
	Protocol string `json:"protocol"`
}

// The root helper observes frames on its own VM attachment. This evidence is
// cleared whenever that attachment's control connection closes or is replaced.
type NetworkSlotSource struct {
	Slot           string                 `json:"slot"`
	Connected      bool                   `json:"connected"`
	ReservedIP     string                 `json:"reserved_ip"`
	ReservedMAC    string                 `json:"reserved_mac"`
	ObservedSource *NetworkObservedSource `json:"observed_source"`
}

func sameMAC(left, right string) bool {
	l, lerr := net.ParseMAC(left)
	r, rerr := net.ParseMAC(right)
	return lerr == nil && rerr == nil && len(l) == 6 && len(r) == 6 && l.String() == r.String()
}

func (status NetworkStatus) verifySlotSource(pref SlotPreference) error {
	if len(status.Connected) != 2 || len(status.SlotSources) != 2 {
		return errors.New("mac network helper lacks current per-connection source evidence; update the helper before first SSH trust")
	}
	index := -1
	for i, expectedName := range []string{"mac1", "mac2"} {
		if status.SlotSources[i].Slot != expectedName {
			return errors.New("mac network source evidence has invalid slot identities")
		}
		if pref.Name == expectedName {
			index = i
		}
	}
	if index < 0 || net.ParseIP(pref.IP).To4() == nil || !sameMAC(pref.MAC, pref.MAC) {
		return errors.New("invalid saved guest network identity; refusing first SSH trust")
	}
	source := status.SlotSources[index]
	if !status.Connected[index] || !source.Connected {
		return fmt.Errorf("%s network attachment is disconnected; refusing first SSH trust", pref.Name)
	}
	if source.ReservedIP != pref.IP || !sameMAC(source.ReservedMAC, pref.MAC) {
		return fmt.Errorf("%s helper reservation does not match saved IP/MAC; refusing first SSH trust", pref.Name)
	}
	observed := source.ObservedSource
	if observed == nil {
		return fmt.Errorf("%s has no observed packet from its reserved IP/MAC yet; refusing first SSH trust", pref.Name)
	}
	if observed.IP != pref.IP || !sameMAC(observed.MAC, pref.MAC) || (observed.Protocol != "ipv4" && observed.Protocol != "arp") {
		return fmt.Errorf("%s observed packet source does not match its reserved IP/MAC; refusing first SSH trust", pref.Name)
	}
	return nil
}

// macOS 27 may scrub ARP/sysctl topology queries. Instead of asking for another
// restricted entitlement, use the helper's observations of its own VM frames.
// This narrows first-contact trust; the captured SSH host key becomes the pin.
func (m *Manager) verifyFirstSSHSource(ctx context.Context, slot *Slot, pref SlotPreference) error {
	probe, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	if err := probe.Err(); err != nil {
		return err
	}
	config, err := m.Store.LoadConfig()
	if err != nil {
		return fmt.Errorf("load saved network identity before SSH trust: %w", err)
	}
	saved, err := config.Preference(slot.Name)
	if err != nil {
		return err
	}
	if pref.Name != slot.Name || saved.IP != pref.IP || !sameMAC(saved.MAC, pref.MAC) {
		return errors.New("SSH target does not match saved slot network identity")
	}
	status, err := m.networkStatus(probe, config)
	if err != nil {
		return fmt.Errorf("verify mac network helper before SSH trust: %w", err)
	}
	if err := probe.Err(); err != nil {
		return err
	}
	return status.verifySlotSource(saved)
}
