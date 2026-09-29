package linux

import (
	"context"
	"errors"
	"os/user"
	"testing"

	"github.com/pgsty/barn/internal/execx"
)

type unitStateRunner struct{ output string }

func (runner unitStateRunner) Run(context.Context, string, ...string) (execx.Result, error) {
	return execx.Result{Stdout: []byte(runner.output)}, nil
}

func TestAccessGroupResolutionSkipsUnknownSupplementaryGroups(t *testing.T) {
	t.Parallel()
	names := map[uint32]string{1000: "vonng", 999: "kvm"}
	lookup := func(gid uint32) (string, error) {
		if name, ok := names[gid]; ok {
			return name, nil
		}
		return "", errors.New("unresolvable fixture gid")
	}
	identity := groupIdentity{Primary: 1000, Groups: []uint32{1000, 424242, 999}}
	if got, err := accessGroupForIdentity(identity, lookup); err != nil || got != "kvm" {
		t.Fatalf("access group = %q, %v", got, err)
	}
	delete(names, 999)
	if got, err := accessGroupForIdentity(identity, lookup); err != nil || got != "vonng" {
		t.Fatalf("primary fallback = %q, %v", got, err)
	}
}

func TestSudoGroupIdentityUsesTheNonRootInvokingUser(t *testing.T) {
	t.Parallel()
	account := &user.User{Uid: "1000", Gid: "100", Username: "alice"}
	identity, err := sudoGroupIdentity("1000", "100", account, []string{"100", "999", "unresolvable"})
	if err != nil {
		t.Fatal(err)
	}
	if identity.Primary != 100 || len(identity.Groups) != 3 || identity.Groups[2] != 999 {
		t.Fatalf("sudo identity = %#v", identity)
	}
	root := &user.User{Uid: "0", Gid: "0", Username: "root"}
	if _, err := sudoGroupIdentity("0", "0", root, []string{"0"}); err == nil {
		t.Fatal("root was accepted as the Debian helper access identity")
	}
}

func TestOptionalNetworkManagerMayBeAbsent(t *testing.T) {
	output := "LoadState=not-found\nActiveState=inactive\nSubState=dead\nUnitFileState=\n"
	state, err := discoverOptionalUnitState(context.Background(), unitStateRunner{output: output}, "NetworkManager.service")
	if err != nil || state.LoadState != "not-found" || state.ActiveState != "inactive" {
		t.Fatalf("optional state=%#v err=%v", state, err)
	}
	if _, err := discoverUnitStateMode(context.Background(), unitStateRunner{output: output}, "systemd-networkd.service", false); err == nil {
		t.Fatal("required systemd unit accepted a not-found state")
	}
}

func TestOptionalNetworkManagerStillRejectsMalformedState(t *testing.T) {
	output := "LoadState=not-found\nActiveState=active\nSubState=running\nUnitFileState=\n"
	if _, err := discoverOptionalUnitState(context.Background(), unitStateRunner{output: output}, "NetworkManager.service"); err == nil {
		t.Fatal("malformed optional unit state was accepted")
	}
}
