package engine

import (
	"strconv"
	"strings"
	"testing"
)

func TestSnapshotClockEpochAndFullPrecisionSequence(t *testing.T) {
	a, b := &Engine{}, &Engine{}
	first := strings.Split(a.NextSnapshotVersion(), "/")
	other := strings.Split(b.NextSnapshotVersion(), "/")
	if len(first) != 2 || first[0] == other[0] || first[1] != "1" {
		t.Fatal(first, other)
	}
	a.snapshot.sequence.Store(9007199254740992)
	version := strings.Split(a.NextSnapshotVersion(), "/")
	if version[0] != first[0] || version[1] != "9007199254740993" {
		t.Fatal(version)
	}
	if _, err := strconv.ParseUint(version[1], 10, 64); err != nil {
		t.Fatal(err)
	}
}
