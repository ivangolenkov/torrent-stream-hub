package fastresume

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
)

func TestCodecVersionOneGolden(t *testing.T) {
	c := fixture()
	c.Identity.Root = "payload" // Native separators do not affect this fixture.
	raw, err := Encode(c)
	if err != nil {
		t.Fatal(err)
	}
	// Independently packed little-endian fixture; pins field order, widths,
	// string lengths, present encoding and LSB bitmap for schema version 1.
	if len(raw) != 442 || fmt.Sprintf("%x", sha256.Sum256(raw)) != "7f8516917de5822f852c4a112d6d30e1e174b50bfb88489c8680801d39676efe" {
		t.Fatal("schema-1 binary envelope changed without a version decision")
	}
}

func TestCodecRoundTrip(t *testing.T) {
	c := fixture()
	c.Generation = math.MaxInt64
	data, err := Encode(c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(data)
	if err != nil || !reflect.DeepEqual(got, c) {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	for i := range data {
		data[i] = 0
	}
	if !reflect.DeepEqual(got, c) {
		t.Fatal("decoded checkpoint aliases serialized bytes")
	}
	if _, err := Encode(nil); !errors.Is(err, ErrInvalid) {
		t.Fatal("nil snapshot encoded", err)
	}
}

func TestCodecTruncationAndBudgets(t *testing.T) {
	data, err := Encode(fixture())
	if err != nil {
		t.Fatal(err)
	}
	for size := 0; size < len(data); size++ {
		if _, err := Decode(data[:size]); err == nil {
			t.Fatalf("truncated snapshot accepted at %d/%d", size, len(data))
		}
	}
	if _, err := Decode(append(bytes.Clone(data), 0)); !errors.Is(err, ErrInvalid) {
		t.Fatal("trailing garbage accepted", err)
	}
	if _, err := Decode(make([]byte, MaxCheckpointBytes+1)); !errors.Is(err, ErrBudget) {
		t.Fatal("oversized envelope accepted", err)
	}
	bad := bytes.Clone(data)
	binary.LittleEndian.PutUint32(bad[32:], math.MaxUint32)
	if _, err := Decode(bad); !errors.Is(err, ErrBudget) {
		t.Fatal("oversized string prefix accepted", err)
	}
	// The variable-size identity precedes bounded counts. No attacker-selected
	// count is converted to int or passed to make before the numerical check.
	d := decoder{data: data}
	d.take(32)
	for range 7 {
		d.text(MaxPathLength)
	}
	d.take(8 * 3)
	pieceOffset := len(data) - len(d.data)
	for _, offset := range []int{pieceOffset, pieceOffset + 16} {
		bad = bytes.Clone(data)
		binary.LittleEndian.PutUint64(bad[offset:], math.MaxInt64)
		if _, err := Decode(bad); !errors.Is(err, ErrBudget) {
			t.Fatalf("oversized count accepted at %d: %v", offset, err)
		}
	}
	if _, err := BitmapBytes(-1); err == nil {
		t.Fatal("negative bitmap size")
	}
	if n, err := BitmapBytes(MaxPieces); err != nil || n != MaxPieces/8 {
		t.Fatal(n, err)
	}
}

func FuzzDecodeCheckpoint(f *testing.F) {
	data, err := Encode(fixture())
	if err != nil {
		f.Fatal(err)
	}
	f.Add(data)
	f.Add([]byte(envelopeMagic))
	f.Fuzz(func(t *testing.T, raw []byte) {
		c, err := Decode(raw)
		if err != nil {
			return
		}
		if err := c.Validate(); err != nil {
			t.Fatal("decoder returned invalid snapshot", err)
		}
		encoded, err := Encode(c)
		if err != nil || !bytes.Equal(raw, encoded) {
			t.Fatal("noncanonical snapshot accepted", err)
		}
	})
}
