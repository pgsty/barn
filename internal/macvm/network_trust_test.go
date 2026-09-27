package macvm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sourceStatusFixture = `{
  "connected":[true,false],
  "slot_sources":[
    {"slot":"mac1","connected":true,"reserved_ip":"10.10.20.10","reserved_mac":"02:aa:bb:cc:00:01","observed_source":{"ip":"10.10.20.10","mac":"02:aa:bb:cc:00:01","protocol":"ipv4"}},
    {"slot":"mac2","connected":false,"reserved_ip":"10.10.20.11","reserved_mac":"02:aa:bb:cc:00:02","observed_source":null}
  ]
}`

func TestFirstSSHTrustRequiresCurrentReservedPacketSource(t *testing.T) {
	pref := SlotPreference{Name: "mac1", IP: "10.10.20.10", MAC: "02:aa:bb:cc:00:01"}
	for _, tc := range []struct {
		name   string
		change func(*NetworkStatus)
		pass   bool
	}{
		{"observed IPv4", func(*NetworkStatus) {}, true},
		{"observed ARP", func(s *NetworkStatus) { s.SlotSources[0].ObservedSource.Protocol = "arp" }, true},
		{"old helper", func(s *NetworkStatus) { s.SlotSources = nil }, false},
		{"no packet yet", func(s *NetworkStatus) { s.SlotSources[0].ObservedSource = nil }, false},
		{"released attachment", func(s *NetworkStatus) {
			s.Connected[0] = false
			s.SlotSources[0].Connected = false
			s.SlotSources[0].ObservedSource = nil
		}, false},
		{"stale source after close", func(s *NetworkStatus) { s.SlotSources[0].Connected = false }, false},
		{"top-level disconnected", func(s *NetworkStatus) { s.Connected[0] = false }, false},
		{"wrong slot", func(s *NetworkStatus) { s.SlotSources[0].Slot = "mac2" }, false},
		{"duplicate slot", func(s *NetworkStatus) { s.SlotSources[1].Slot = "mac1" }, false},
		{"wrong reservation IP", func(s *NetworkStatus) { s.SlotSources[0].ReservedIP = "10.10.20.11" }, false},
		{"wrong reservation MAC", func(s *NetworkStatus) { s.SlotSources[0].ReservedMAC = "02:aa:bb:cc:00:02" }, false},
		{"other IP same MAC", func(s *NetworkStatus) { s.SlotSources[0].ObservedSource.IP = "10.10.20.11" }, false},
		{"same IP other MAC", func(s *NetworkStatus) { s.SlotSources[0].ObservedSource.MAC = "02:aa:bb:cc:00:02" }, false},
		{"MAC substring", func(s *NetworkStatus) { s.SlotSources[0].ObservedSource.MAC = "02:aa:bb:cc:00:01:00:00" }, false},
		{"DHCP reservation alone", func(s *NetworkStatus) { s.SlotSources[0].ObservedSource.Protocol = "dhcp" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var status NetworkStatus
			if err := json.Unmarshal([]byte(sourceStatusFixture), &status); err != nil {
				t.Fatal(err)
			}
			tc.change(&status)
			if err := status.verifySlotSource(pref); (err == nil) != tc.pass {
				t.Fatalf("pass=%v error=%v", tc.pass, err)
			}
		})
	}
}

func TestFirstSSHTrustChecksSavedConfigurationHelperIdentityAndCancellation(t *testing.T) {
	s := testStore(t)
	held := holdStore(t, s)
	config, err := NewConfig(DefaultSubnet)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveConfig(held, config); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	m := &Manager{Store: s, Runner: Runner{Binary: filepath.Join(bin, "farrow-mac-runner")}}
	socket, err := m.networkSocket(config)
	if err != nil {
		t.Fatal(err)
	}
	pref, err := config.Preference("mac1")
	if err != nil {
		t.Fatal(err)
	}
	status := NetworkStatus{OK: true, UID: os.Getuid(), InstallationID: config.InstallationID, Subnet: config.Network.Subnet, Socket: socket, Connected: []bool{true, false}}
	for _, p := range config.Slots {
		source := NetworkSlotSource{Slot: p.Name, Connected: p.Name == "mac1", ReservedIP: p.IP, ReservedMAC: p.MAC}
		if source.Connected {
			source.ObservedSource = &NetworkObservedSource{IP: p.IP, MAC: p.MAC, Protocol: "ipv4"}
		}
		status.SlotSources = append(status.SlotSources, source)
	}
	writeHelper := func(s NetworkStatus) {
		t.Helper()
		wire, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		script := "#!/bin/sh\n" + QuoteCommand([]string{"printf", "%s\\n", string(wire)}) + "\n"
		if err := os.WriteFile(filepath.Join(bin, "farrow-mac-network"), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeHelper(status)
	slot := &Slot{Name: "mac1"}
	if err := m.verifyFirstSSHSource(context.Background(), slot, pref); err != nil {
		t.Fatal(err)
	}
	wrongPref := pref
	wrongPref.IP = "10.10.20.200"
	if err := m.verifyFirstSSHSource(context.Background(), slot, wrongPref); err == nil {
		t.Fatal("accepted IP outside saved slot")
	}
	status.InstallationID = "00000000-0000-4000-8000-000000000000"
	writeHelper(status)
	if err := m.verifyFirstSSHSource(context.Background(), slot, pref); err == nil || !strings.Contains(err.Error(), "identity/configuration mismatch") {
		t.Fatalf("accepted foreign helper: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.verifyFirstSSHSource(ctx, slot, pref); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
}
