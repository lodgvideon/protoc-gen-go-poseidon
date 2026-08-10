package pgrpc

import (
	"testing"
	"unsafe"
)

// TestBinValuesAliasTheCurrentArena is what actually pins the span indirection.
//
// The external test only checks that binary values still READ correctly after
// the arena grows, and that passes either way: binding a value at set time
// leaves it pointing into the abandoned array, which Go keeps alive and
// unmodified. Nothing is corrupted.
//
// What binding at set time costs is memory. Each entry holds its own arena
// generation alive, so a build that grows the arena k times pins k arenas, and
// the reuse this type exists for silently stops happening. The only way to
// observe that is from inside the package, by checking that every value lies
// within the CURRENT arena's backing array.
func TestBinValuesAliasTheCurrentArena(t *testing.T) {
	var m Metadata
	if err := m.AddBin("a-bin", []byte{1, 2, 3}); err != nil {
		t.Fatalf("AddBin: %v", err)
	}
	for i := range 8 {
		if err := m.AddBin("b-bin", make([]byte, 512<<i)); err != nil {
			t.Fatalf("AddBin big #%d: %v", i, err)
		}
	}

	fields := m.Fields()
	arena := m.bin[:cap(m.bin)]
	lo := uintptr(unsafe.Pointer(unsafe.SliceData(arena)))
	hi := lo + uintptr(cap(arena))

	pinned := 0
	for i := range fields {
		// Selected by NAME, not by m.spans[i].n. Filtering on the span would
		// use the very field a broken implementation gets wrong, so a bug that
		// marks binary entries as textual would make this loop skip exactly the
		// entries it exists to check.
		if !hasBinSuffix(fields[i].Name) {
			continue
		}
		p := uintptr(unsafe.Pointer(unsafe.SliceData(fields[i].Value)))
		if p < lo || p >= hi {
			pinned++
			t.Errorf("field %d (%q) points outside the current arena; it pins an abandoned one",
				i, fields[i].Name)
		}
	}
	if pinned > 0 {
		t.Logf("%d of %d entries hold a stale arena alive", pinned, len(fields))
	}
}

// TestResetKeepsArenaCapacity guards the other half of the bargain: Reset must
// wipe the bytes without surrendering the array, or the next build reallocates
// and the builder is no cheaper than grpc.AppendMetadata.
func TestResetKeepsArenaCapacity(t *testing.T) {
	var m Metadata
	if err := m.SetBin("k-bin", make([]byte, 1024)); err != nil {
		t.Fatalf("SetBin: %v", err)
	}
	before := cap(m.bin)
	if before == 0 {
		t.Fatal("the arena never grew")
	}

	m.Reset()

	if cap(m.bin) != before {
		t.Errorf("arena capacity %d after Reset, want %d kept", cap(m.bin), before)
	}
	if len(m.bin) != 0 {
		t.Errorf("arena length %d after Reset, want 0", len(m.bin))
	}
}

// hasBinSuffix reports whether an already-lowercased field name ends in "-bin".
// It lives in the test file because production code has no need for it: the
// only caller that must classify an entry uses its span.
func hasBinSuffix(name []byte) bool {
	if len(name) < len(binSuffix) {
		return false
	}
	return string(name[len(name)-len(binSuffix):]) == binSuffix
}
