package failure

import (
	"errors"
	"fmt"
	"testing"
)

func TestClassifyOutermostClassAndFirstNext(t *testing.T) {
	inner := New(Integrity, errors.New("digest mismatch")).Because("digest").Then("barn image pull u24")
	outer := New(Conflict, fmt.Errorf("start node a: %w", inner)).Because("node_state")
	class, reason, next := Classify(fmt.Errorf("up: %w", outer))
	if class != Conflict || reason != "node_state" || next != "barn image pull u24" {
		t.Fatalf("Classify = %q %q %q", class, reason, next)
	}
}

func TestClassifyWalksJoinedErrors(t *testing.T) {
	joined := errors.Join(errors.New("plain"), WithNext(New(Capability, errors.New("qemu missing")), "barn setup"))
	class, _, next := Classify(joined)
	if class != Capability || next != "barn setup" {
		t.Fatalf("Classify = %q %q", class, next)
	}
}

func TestUnclassifiedAndMessage(t *testing.T) {
	if class, _, _ := Classify(errors.New("x")); class != "" {
		t.Fatalf("unclassified error got %q", class)
	}
	err := New(Usage, errors.New("bad flag")).Then("barn --help")
	if err.Error() != "bad flag" {
		t.Fatalf("message changed: %q", err.Error())
	}
	if WithNext(nil, "x") != nil {
		t.Fatal("WithNext(nil) must be nil")
	}
}
