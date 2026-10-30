package portalloc

import (
	"reflect"
	"testing"
)

func TestCandidates(t *testing.T) {
	t.Parallel()
	want := []uint16{15432, 25432, 35432, 45432, 55432, 65432}
	if got := Candidates(15432); !reflect.DeepEqual(got, want) {
		t.Fatalf("Candidates = %v, want %v", got, want)
	}
	if got := Candidates(60000); !reflect.DeepEqual(got, []uint16{60000}) {
		t.Fatalf("overflow candidates = %v", got)
	}
}

func TestChoose(t *testing.T) {
	t.Parallel()
	got, err := Choose(15432, func(port uint16) bool { return port == 35432 })
	if err != nil {
		t.Fatal(err)
	}
	if got != 35432 {
		t.Fatalf("Choose = %d, want 35432", got)
	}
	if _, err := Choose(15432, func(uint16) bool { return false }); err == nil {
		t.Fatal("exhausted candidate set unexpectedly succeeded")
	}
}
