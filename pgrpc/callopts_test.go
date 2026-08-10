package pgrpc_test

import (
	"errors"
	"testing"
	"unsafe"

	"github.com/lodgvideon/poseidon-http-client/conn"
	"github.com/lodgvideon/poseidon-http-client/grpc"
	"github.com/lodgvideon/protoc-gen-go-poseidon/pgrpc"
)

// sameArray reports whether two slices share a backing array. Comparing the
// data pointers is the only way to test the aliasing rule; comparing contents
// would pass for a copy, which is exactly what must be distinguished.
func sameArray(a, b []conn.HeaderField) bool {
	if cap(a) == 0 || cap(b) == 0 {
		return false
	}
	return unsafe.SliceData(a) == unsafe.SliceData(b)
}

// mdOf builds metadata with SPARE CAPACITY on purpose. The aliasing tests need
// an unguarded append to have somewhere to land in place — with a
// tightly-sized slice every append reallocates and they would pass without
// exercising anything.
func mdOf(t testing.TB, pairs ...string) []conn.HeaderField {
	t.Helper()
	md := make([]conn.HeaderField, 0, len(pairs)/2+4)
	for i := 0; i < len(pairs); i += 2 {
		next, err := grpc.AppendMetadata(md, pairs[i], []byte(pairs[i+1]))
		if err != nil {
			t.Fatalf("AppendMetadata(%q): %v", pairs[i], err)
		}
		md = next
	}
	return md
}

func TestWithMetadataInstallsByReference(t *testing.T) {
	md := mdOf(t, "x-tenant", "acme")
	var cfg pgrpc.CallConfig
	cfg.Apply(pgrpc.WithMetadata(md))

	if !sameArray(cfg.MD, md) {
		t.Error("WithMetadata copied; it is documented to install by reference")
	}
}

// TestAppendAfterWithMetadataDoesNotTouchCallerMemory is the defect this whole
// mechanism exists to prevent. poseidon's buildHeaders reads md[i]
// synchronously inside NewStream, so appending into a borrowed array ships one
// virtual user's header — routinely a credential — on another user's RPC.
func TestAppendAfterWithMetadataDoesNotTouchCallerMemory(t *testing.T) {
	shared := mdOf(t, "x-tenant", "acme")

	var cfg pgrpc.CallConfig
	cfg.Apply(
		pgrpc.WithMetadata(shared),
		pgrpc.WithHeaderString("x-request-id", "r-1"),
	)
	if cfg.Err != nil {
		t.Fatalf("Apply: %v", cfg.Err)
	}

	if sameArray(cfg.MD, shared) {
		t.Fatal("appended into the caller's array; a concurrent RPC reading it would see a foreign header")
	}
	if len(shared) != 1 {
		t.Errorf("caller's slice grew to %d entries", len(shared))
	}
	if len(cfg.MD) != 2 {
		t.Fatalf("cfg.MD has %d entries, want 2", len(cfg.MD))
	}
}

// TestHandAssignedMetadataIsTreatedAsBorrowed covers the path with no option at
// all: CallConfig.MD is exported and documented as directly assignable, so the
// zero value of the ownership flag has to mean "borrowed" or that path silently
// loses the protection.
func TestHandAssignedMetadataIsTreatedAsBorrowed(t *testing.T) {
	shared := mdOf(t, "a", "1")

	cfg := pgrpc.CallConfig{MD: shared}
	cfg.Apply(pgrpc.WithHeaderString("b", "2"))
	if cfg.Err != nil {
		t.Fatalf("Apply: %v", cfg.Err)
	}
	if sameArray(cfg.MD, shared) {
		t.Error("a hand-assigned MD was appended in place")
	}
}

func TestSecondAppendReusesTheAdoptedArray(t *testing.T) {
	var cfg pgrpc.CallConfig
	cfg.Apply(pgrpc.WithHeaderString("a", "1"))
	first := cfg.MD
	cfg.Apply(pgrpc.WithHeaderString("b", "2"))

	if len(cfg.MD) != 2 {
		t.Fatalf("got %d entries, want 2", len(cfg.MD))
	}
	if cap(first) >= 2 && !sameArray(cfg.MD, first) {
		t.Error("copied again on the second append; adoption should happen once")
	}
}

// TestWithHeaderKeepsEarlierEntriesOnFailure pins the two-step append.
// grpc.AppendMetadata returns (nil, err), so the idiomatic single-assignment
// form would silently discard every header accumulated before the bad one.
func TestWithHeaderKeepsEarlierEntriesOnFailure(t *testing.T) {
	var cfg pgrpc.CallConfig
	cfg.Apply(
		pgrpc.WithHeaderString("x-good", "1"),
		pgrpc.WithHeaderString("content-type", "application/grpc+json"), // reserved
		pgrpc.WithHeaderString("x-also-good", "2"),
	)

	if cfg.Err == nil {
		t.Fatal("a reserved key was accepted")
	}
	if !errors.Is(cfg.Err, grpc.ErrReservedMetadata) {
		t.Errorf("Err = %v, want ErrReservedMetadata", cfg.Err)
	}
	if len(cfg.MD) != 2 {
		t.Errorf("kept %d entries, want 2 — the failing append destroyed the others", len(cfg.MD))
	}
}

func TestErrLatchesTheFirstFailure(t *testing.T) {
	var cfg pgrpc.CallConfig
	cfg.Apply(
		pgrpc.WithHeaderString("te", "trailers"),  // reserved
		pgrpc.WithHeaderString(":method", "POST"), // pseudo-header
	)
	if cfg.Err == nil {
		t.Fatal("no error latched")
	}
	if !errors.Is(cfg.Err, grpc.ErrReservedMetadata) {
		t.Errorf("Err = %v, want the FIRST failure", cfg.Err)
	}
}

func TestResetClearsOwnedMetadataAndKeepsTheArray(t *testing.T) {
	var cfg pgrpc.CallConfig
	cfg.Apply(pgrpc.WithHeaderString("authorization", "Bearer secret"))
	owned := cfg.MD[:cap(cfg.MD)]

	cfg.Reset()

	if len(cfg.MD) != 0 {
		t.Errorf("MD has %d entries after Reset", len(cfg.MD))
	}
	for i := range owned {
		if owned[i].Name != nil || owned[i].Value != nil {
			t.Errorf("entry %d still reachable after Reset: %q", i, owned[i].Value)
		}
	}
	if cap(cfg.MD) == 0 {
		t.Error("Reset dropped the backing array it owned")
	}
}

// TestResetDoesNotClearBorrowedMetadata is the mirror image, and the more
// dangerous direction: clearing a borrowed slice zeroes metadata its owner is
// still holding and expecting to reuse on the next RPC.
func TestResetDoesNotClearBorrowedMetadata(t *testing.T) {
	shared := mdOf(t, "x-tenant", "acme")
	var cfg pgrpc.CallConfig
	cfg.Apply(pgrpc.WithMetadata(shared))

	cfg.Reset()

	if shared[0].Name == nil || string(shared[0].Value) != "acme" {
		t.Fatal("Reset wiped the caller's own metadata")
	}
	if cfg.MD != nil {
		t.Errorf("MD = %v, want nil after dropping a borrowed slice", cfg.MD)
	}

	// And the dropped alias must not come back: appending now must allocate
	// rather than write into the caller's array.
	cfg.Apply(pgrpc.WithHeaderString("b", "2"))
	if sameArray(cfg.MD, shared) {
		t.Error("append after Reset landed back in the caller's array")
	}
}

func TestResetClearsPassAndCodecAndErr(t *testing.T) {
	var cfg pgrpc.CallConfig
	cfg.Apply(
		pgrpc.MaxRecvMessageSize(1<<20),
		pgrpc.WithCallCodec(stubCodec{}),
		pgrpc.WithHeaderString("te", "trailers"), // fails, latches Err
	)
	if cfg.Err == nil || len(cfg.Pass) == 0 || cfg.Codec == nil {
		t.Fatalf("setup did not populate the config: %+v", cfg)
	}

	cfg.Reset()

	if cfg.Err != nil {
		t.Errorf("Err = %v after Reset", cfg.Err)
	}
	if len(cfg.Pass) != 0 {
		t.Errorf("Pass has %d entries after Reset", len(cfg.Pass))
	}
	if cfg.Codec != nil {
		t.Error("Codec survived Reset")
	}
}

func TestPassAccumulates(t *testing.T) {
	var cfg pgrpc.CallConfig
	cfg.Apply(
		pgrpc.MaxRecvMessageSize(1<<20),
		pgrpc.WithPoseidonCallOptions(grpc.MaxRecvMessageSize(1<<21)),
	)
	if len(cfg.Pass) != 2 {
		t.Errorf("Pass has %d entries, want 2 — options must accumulate, not replace", len(cfg.Pass))
	}
}

func TestApplyIgnoresNilOptions(t *testing.T) {
	var cfg pgrpc.CallConfig
	cfg.Apply(nil, pgrpc.WithHeaderString("a", "1"), nil)
	if cfg.Err != nil {
		t.Fatalf("Err = %v", cfg.Err)
	}
	if len(cfg.MD) != 1 {
		t.Errorf("got %d entries, want 1", len(cfg.MD))
	}
}

func TestLaterScalarOptionWins(t *testing.T) {
	first, second := stubCodec{name: "first"}, stubCodec{name: "second"}
	var cfg pgrpc.CallConfig
	cfg.Apply(pgrpc.WithCallCodec(first), pgrpc.WithCallCodec(second))
	if cfg.Codec.Name() != "second" {
		t.Errorf("Codec = %q, want the later option to win", cfg.Codec.Name())
	}
}

// stubCodec is a Codec that does nothing, for tests that only care which codec
// was selected.
type stubCodec struct{ name string }

func (c stubCodec) MarshalAppend(dst []byte, _ any) ([]byte, error) { return dst, nil }
func (c stubCodec) Unmarshal(_ []byte, _ any) error                 { return nil }
func (c stubCodec) Name() string {
	if c.name == "" {
		return "stub"
	}
	return c.name
}
