package pgrpc_test

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"unsafe"

	"github.com/lodgvideon/poseidon-http-client/conn"
	"github.com/lodgvideon/poseidon-http-client/grpc"
	"github.com/lodgvideon/protoc-gen-go-poseidon/pgrpc"
)

func valueOf(t *testing.T, fields []conn.HeaderField, name string) string {
	t.Helper()
	for i := range fields {
		if string(fields[i].Name) == name {
			return string(fields[i].Value)
		}
	}
	t.Fatalf("no field named %q in %v", name, fields)
	return ""
}

func TestMetadataLowercasesAndBuilds(t *testing.T) {
	var m pgrpc.Metadata
	if err := m.SetText("X-Tenant", []byte("acme")); err != nil {
		t.Fatalf("SetText: %v", err)
	}
	f := m.Fields()
	if len(f) != 1 {
		t.Fatalf("got %d fields, want 1", len(f))
	}
	if string(f[0].Name) != "x-tenant" {
		t.Errorf("name = %q, want it lowercased", f[0].Name)
	}
	if string(f[0].Value) != "acme" {
		t.Errorf("value = %q", f[0].Value)
	}
}

// TestMetadataRejectsRawValueForBinKey is the one gap poseidon cannot close. A
// raw ASCII value in a "-bin" field passes every validator on the wire, and the
// server then base64-decodes garbage.
func TestMetadataRejectsRawValueForBinKey(t *testing.T) {
	var m pgrpc.Metadata
	err := m.SetText("trace-bin", []byte("not-base64"))
	if err == nil {
		t.Fatal("a raw value was accepted for a -bin key")
	}
	if !strings.Contains(err.Error(), "SetBin") {
		t.Errorf("error does not say what to use instead: %v", err)
	}
	if len(m.Fields()) != 0 {
		t.Error("the rejected entry was still added")
	}
}

func TestMetadataRejectsBinCallOnTextKey(t *testing.T) {
	var m pgrpc.Metadata
	if err := m.SetBin("x-tenant", []byte("acme")); err == nil {
		t.Fatal("SetBin accepted a key that does not end in -bin")
	}
}

// TestMetadataValidatesKeysThroughPoseidon pins the decision to intern via
// grpc.AppendMetadata rather than reimplementing the rules. A hand-rolled copy
// would drift the moment poseidon reserves another header.
func TestMetadataValidatesKeysThroughPoseidon(t *testing.T) {
	for _, key := range []string{"content-type", "te", "grpc-timeout", "grpc-anything", ":method"} {
		t.Run(key, func(t *testing.T) {
			var m pgrpc.Metadata
			err := m.SetText(key, []byte("x"))
			if err == nil {
				t.Fatalf("%q was accepted", key)
			}
			if !errors.Is(err, grpc.ErrReservedMetadata) && !errors.Is(err, grpc.ErrInvalidMetadata) {
				t.Errorf("err = %v, want a poseidon metadata sentinel", err)
			}
		})
	}
}

func TestMetadataBinRoundTrips(t *testing.T) {
	raw := []byte{0x00, 0xFF, 0x10, 0x0A}
	var m pgrpc.Metadata
	if err := m.SetBin("trace-bin", raw); err != nil {
		t.Fatalf("SetBin: %v", err)
	}

	got := valueOf(t, m.Fields(), "trace-bin")
	if want := base64.StdEncoding.EncodeToString(raw); got != want {
		t.Errorf("encoded = %q, want %q", got, want)
	}

	back, ok, err := pgrpc.Get(m.Fields(), "trace-bin")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if string(back) != string(raw) {
		t.Errorf("round trip = % x, want % x", back, raw)
	}
}

// TestMetadataBinValuesSurviveArenaGrowth checks the user-visible half: values
// still read back correctly after the arena has reallocated several times.
//
// It does NOT discriminate the span indirection — binding a value at set time
// also reads back correctly, because the abandoned array stays alive and
// unmodified. What the spans buy is that no entry pins a stale arena, and that
// is only observable from inside the package; see
// TestBinValuesAliasTheCurrentArena in metadata_internal_test.go.
func TestMetadataBinValuesSurviveArenaGrowth(t *testing.T) {
	var m pgrpc.Metadata
	first := []byte{1, 2, 3}
	if err := m.AddBin("a-bin", first); err != nil {
		t.Fatalf("AddBin: %v", err)
	}
	// Force several reallocations of the arena.
	for i := range 8 {
		big := make([]byte, 512<<i)
		for j := range big {
			big[j] = byte(i + 1)
		}
		if err := m.AddBin("b-bin", big); err != nil {
			t.Fatalf("AddBin big #%d: %v", i, err)
		}
	}

	got := valueOf(t, m.Fields(), "a-bin")
	if want := base64.StdEncoding.EncodeToString(first); got != want {
		t.Errorf("the first value was corrupted by arena growth:\n got %q\nwant %q", got, want)
	}
}

func TestMetadataSetReplacesAndAddAppends(t *testing.T) {
	var m pgrpc.Metadata
	if err := m.SetText("k", []byte("1")); err != nil {
		t.Fatal(err)
	}
	if err := m.SetText("k", []byte("2")); err != nil {
		t.Fatal(err)
	}
	if got := m.Fields(); len(got) != 1 || string(got[0].Value) != "2" {
		t.Errorf("Set did not replace: %v", got)
	}

	if err := m.AddText("k", []byte("3")); err != nil {
		t.Fatal(err)
	}
	if got := m.Fields(); len(got) != 2 {
		t.Errorf("Add did not append: %v", got)
	}
}

// TestMetadataSetBinReplaceKeepsTheRightBytes covers replace-plus-arena
// together: the replaced entry must read back as the NEW value, not the old
// span.
func TestMetadataSetBinReplaceKeepsTheRightBytes(t *testing.T) {
	var m pgrpc.Metadata
	if err := m.SetBin("k-bin", []byte{1, 1, 1, 1}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetBin("k-bin", []byte{9, 9}); err != nil {
		t.Fatal(err)
	}
	f := m.Fields()
	if len(f) != 1 {
		t.Fatalf("got %d fields, want 1", len(f))
	}
	back, _, err := pgrpc.Get(f, "k-bin")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(back) != string([]byte{9, 9}) {
		t.Errorf("value = % x, want the replacement", back)
	}
}

func TestSetSensitiveMarksNeverIndexed(t *testing.T) {
	var m pgrpc.Metadata
	if err := m.SetSensitive("x-api-key", []byte("secret")); err != nil {
		t.Fatalf("SetSensitive: %v", err)
	}
	f := m.Fields()
	if f[0].Indexing != pgrpc.IndexNever {
		t.Errorf("Indexing = %v, want IndexNever — the value would enter the shared HPACK table",
			f[0].Indexing)
	}
	if err := m.SetText("x-plain", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if m.Fields()[1].Indexing != pgrpc.IndexIncremental {
		t.Error("SetText should not mark a field never-indexed")
	}
}

// TestMetadataResetClearsCredentials mirrors poseidon's putHeaderScratch rule.
// A reused builder is the wrong place to leave a bearer token reachable after
// the RPC that carried it.
func TestMetadataResetClearsCredentials(t *testing.T) {
	var m pgrpc.Metadata
	if err := m.SetSensitive("authorization", []byte("Bearer hunter2")); err != nil {
		t.Fatal(err)
	}
	if err := m.SetBin("token-bin", []byte("binary-secret")); err != nil {
		t.Fatal(err)
	}
	fields := m.Fields()
	held := fields[:cap(fields)]
	arena := m.Fields()[1].Value

	m.Reset()

	if len(m.Fields()) != 0 {
		t.Fatalf("Reset left %d fields", len(m.Fields()))
	}
	for i := range held {
		if held[i].Name != nil || held[i].Value != nil {
			t.Errorf("field %d still reachable after Reset", i)
		}
	}
	for i := range arena {
		if arena[i] != 0 {
			t.Errorf("the base64 arena still holds the encoded secret: %q", arena)
			break
		}
	}
}

// TestMetadataIsAllocationFreeInSteadyState is the entire justification for
// this type existing. If it allocates on a rebuild, use grpc.AppendMetadata and
// delete it.
func TestMetadataIsAllocationFreeInSteadyState(t *testing.T) {
	// The values are hoisted out of the closure deliberately. A []byte("…")
	// conversion inside it would be charged to the measurement — SetText
	// RETAINS the slice, so it escapes to the heap — and it would be the test's
	// allocation, not the builder's. A real caller holds its buffers outside
	// the request loop for the same reason.
	var (
		m      pgrpc.Metadata
		tenant = []byte("acme")
		token  = []byte("Bearer t")
		trace  = []byte{1, 2, 3, 4, 5, 6, 7, 8}
	)
	build := func() {
		m.Reset()
		// Mixed case on purpose: a lowercase key would let a ToLower-based
		// suffix check pass this test for free.
		if err := m.SetText("X-Tenant", tenant); err != nil {
			t.Fatal(err)
		}
		if err := m.SetSensitive("authorization", token); err != nil {
			t.Fatal(err)
		}
		if err := m.SetBin("trace-bin", trace); err != nil {
			t.Fatal(err)
		}
		_ = m.Fields()
	}
	build() // intern the keys and grow the arena

	if got := testing.AllocsPerRun(200, build); got != 0 {
		t.Errorf("%v allocations per rebuild, want 0", got)
	}
}

func TestMetadataInterningSurvivesReset(t *testing.T) {
	var m pgrpc.Metadata
	if err := m.SetText("x-tenant", []byte("a")); err != nil {
		t.Fatal(err)
	}
	name := unsafe.SliceData(m.Fields()[0].Name)

	m.Reset()
	if err := m.SetText("x-tenant", []byte("b")); err != nil {
		t.Fatal(err)
	}
	if unsafe.SliceData(m.Fields()[0].Name) != name {
		t.Error("the key was re-interned after Reset; that is the one thing this builder saves")
	}
}

func TestMetadataFeedsWithMetadata(t *testing.T) {
	var m pgrpc.Metadata
	if err := m.SetText("x-tenant", []byte("acme")); err != nil {
		t.Fatal(err)
	}
	var cfg pgrpc.CallConfig
	cfg.Apply(pgrpc.WithMetadata(m.Fields()))
	if len(cfg.MD) != 1 || string(cfg.MD[0].Name) != "x-tenant" {
		t.Errorf("CallConfig.MD = %v", cfg.MD)
	}
}
