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
// data pointers is the only way to test the ownership rule; comparing contents
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

func names(md []conn.HeaderField) []string {
	out := make([]string, len(md))
	for i := range md {
		out[i] = string(md[i].Name)
	}
	return out
}

// The three tests below are the ones that were missing, and each corresponds to
// a way the previous ownership latch failed. It was a one-way bool: once
// anything adopted, nothing could mark the metadata borrowed again. Every case
// here reproduced a wrong header on the wire before the owner-identity fix.

// TestSetMetadataAfterAdoptionIsBorrowedAgain is the first. Under the old latch
// this config had already adopted, so the freshly installed caller slice was
// treated as its own and the next append landed in the caller's array.
func TestSetMetadataAfterAdoptionIsBorrowedAgain(t *testing.T) {
	var cfg pgrpc.CallConfig
	cfg.Apply(pgrpc.WithHeaderString("first", "1")) // adopts

	shared := mdOf(t, "x-tenant", "acme")
	cfg.SetMetadata(shared)
	cfg.Apply(pgrpc.WithHeaderString("x-request-id", "r-1"))
	if err := cfg.Err(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if sameArray(cfg.Metadata(), shared) {
		t.Fatal("appended into the caller's array after re-installing it")
	}
	if len(shared) != 1 {
		t.Errorf("the caller's slice grew to %d entries: %v", len(shared), names(shared))
	}
}

// TestResetDoesNotClearAReinstalledSlice is the second. A zero config's Reset
// used to mark it owning, so a later SetMetadata plus Reset ran clear() over
// memory belonging to whoever built it.
func TestResetDoesNotClearAReinstalledSlice(t *testing.T) {
	var cfg pgrpc.CallConfig
	cfg.Reset() // the step that used to flip ownership on

	shared := mdOf(t, "authorization", "Bearer secret")
	cfg.SetMetadata(shared)
	cfg.Reset()

	if shared[0].Name == nil || string(shared[0].Value) != "Bearer secret" {
		t.Fatalf("Reset wiped the caller's own metadata: %+v", shared[0])
	}
}

// TestValueCopyDoesNotAlias is the third and the one that reached the wire. The
// Caller's own documentation prescribes one config per virtual user built from
// a shared base; under a copied bool latch both copies believed they owned the
// base's array and appended into it, so one user's credential shipped on
// another's request.
//
// go vet is silent here: CallConfig holds no lock, so copylocks never fires.
func TestValueCopyDoesNotAlias(t *testing.T) {
	// THREE headers, not one, and that is the whole test.
	//
	// A single Apply leaves len=1 cap=1, so every later append reallocates on
	// its own and the assertion below cannot fail — the first version of this
	// test was exactly that, and mutation-checking caught it: restoring the
	// pre-fix bool latch left it green. Three appends grow the array to
	// len=3 cap=4, so an unguarded append has a spare slot to land in and the
	// aliasing becomes observable.
	var base pgrpc.CallConfig
	base.Apply(
		pgrpc.WithHeaderString("x-run", "run-1"),
		pgrpc.WithHeaderString("x-build", "abc"),
		pgrpc.WithHeaderString("x-region", "eu"),
	)
	if cap(base.Metadata()) <= len(base.Metadata()) {
		t.Fatalf("the base has no spare capacity (len=%d cap=%d); this test cannot detect aliasing",
			len(base.Metadata()), cap(base.Metadata()))
	}

	vu1, vu2 := base, base
	vu1.Apply(pgrpc.WithHeaderString("authorization", "Bearer VU1"))
	vu2.Apply(pgrpc.WithHeaderString("authorization", "Bearer VU2"))

	got1 := vu1.Metadata()
	got2 := vu2.Metadata()
	if sameArray(got1, got2) {
		t.Fatal("two copies share one metadata array")
	}
	if v := string(got1[len(got1)-1].Value); v != "Bearer VU1" {
		t.Errorf("VU1 carries %q — another user's credential", v)
	}
	if v := string(got2[len(got2)-1].Value); v != "Bearer VU2" {
		t.Errorf("VU2 carries %q", v)
	}
	if len(base.Metadata()) != 3 {
		t.Errorf("the base grew to %d entries: %v", len(base.Metadata()), names(base.Metadata()))
	}
}

// TestWithMetadataAppends pins the direction chosen when the code and two doc
// comments disagreed. Replacing would let a per-call option silently strip a
// client-wide credential, which is the worse of the two failures.
func TestWithMetadataAppends(t *testing.T) {
	var cfg pgrpc.CallConfig
	cfg.Apply(pgrpc.WithHeaderString("authorization", "Bearer SERVICE"))
	cfg.Apply(pgrpc.WithMetadata(mdOf(t, "x-request-id", "r-42")))
	if err := cfg.Err(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got := names(cfg.Metadata())
	if len(got) != 2 || got[0] != "authorization" || got[1] != "x-request-id" {
		t.Errorf("metadata = %v, want the default kept and the per-call one appended", got)
	}
}

// TestWithMetadataInstallsByReferenceWhenEmpty keeps the cheap path honest: the
// append semantics must not cost a copy when there is nothing to preserve.
func TestWithMetadataInstallsByReferenceWhenEmpty(t *testing.T) {
	md := mdOf(t, "x-tenant", "acme")
	var cfg pgrpc.CallConfig
	cfg.Apply(pgrpc.WithMetadata(md))

	if !sameArray(cfg.Metadata(), md) {
		t.Error("copied on an empty config; that path is documented as by-reference")
	}
}

func TestAppendAfterWithMetadataDoesNotTouchCallerMemory(t *testing.T) {
	shared := mdOf(t, "x-tenant", "acme")

	var cfg pgrpc.CallConfig
	cfg.Apply(
		pgrpc.WithMetadata(shared),
		pgrpc.WithHeaderString("x-request-id", "r-1"),
	)
	if err := cfg.Err(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if sameArray(cfg.Metadata(), shared) {
		t.Fatal("appended into the caller's array")
	}
	if len(shared) != 1 {
		t.Errorf("the caller's slice grew to %d entries", len(shared))
	}
}

func TestSecondAppendReusesTheAdoptedArray(t *testing.T) {
	var cfg pgrpc.CallConfig
	cfg.Apply(pgrpc.WithHeaderString("a", "1"))
	first := cfg.Metadata()
	cfg.Apply(pgrpc.WithHeaderString("b", "2"))

	if len(cfg.Metadata()) != 2 {
		t.Fatalf("got %d entries, want 2", len(cfg.Metadata()))
	}
	if cap(first) >= 2 && !sameArray(cfg.Metadata(), first) {
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

	if cfg.Err() == nil {
		t.Fatal("a reserved key was accepted")
	}
	if !errors.Is(cfg.Err(), grpc.ErrReservedMetadata) {
		t.Errorf("Err = %v, want ErrReservedMetadata", cfg.Err())
	}
	if len(cfg.Metadata()) != 2 {
		t.Errorf("kept %d entries, want 2 — the failing append destroyed the others", len(cfg.Metadata()))
	}
}

func TestErrLatchesTheFirstFailure(t *testing.T) {
	var cfg pgrpc.CallConfig
	cfg.Apply(
		pgrpc.WithHeaderString("te", "trailers"),  // reserved
		pgrpc.WithHeaderString(":method", "POST"), // pseudo-header
	)
	if cfg.Err() == nil {
		t.Fatal("no error latched")
	}
	if !errors.Is(cfg.Err(), grpc.ErrReservedMetadata) {
		t.Errorf("Err = %v, want the FIRST failure", cfg.Err())
	}
}

func TestResetClearsOwnedMetadataAndKeepsTheArray(t *testing.T) {
	var cfg pgrpc.CallConfig
	cfg.Apply(pgrpc.WithHeaderString("authorization", "Bearer secret"))
	owned := cfg.Metadata()[:cap(cfg.Metadata())]

	cfg.Reset()

	if len(cfg.Metadata()) != 0 {
		t.Errorf("MD has %d entries after Reset", len(cfg.Metadata()))
	}
	for i := range owned {
		if owned[i].Name != nil || owned[i].Value != nil {
			t.Errorf("entry %d still reachable after Reset: %q", i, owned[i].Value)
		}
	}
	if cap(cfg.Metadata()) == 0 {
		t.Error("Reset dropped the backing array it owned")
	}
}

func TestResetClearsPassAndCodecAndErr(t *testing.T) {
	var cfg pgrpc.CallConfig
	cfg.Apply(
		pgrpc.MaxRecvMessageSize(1<<20),
		pgrpc.WithCallCodec(stubCodec{}),
		pgrpc.WithHeaderString("te", "trailers"), // fails, latches Err
	)
	if cfg.Err() == nil || len(cfg.PoseidonOptions()) == 0 || cfg.Codec() == nil {
		t.Fatal("setup did not populate the config")
	}

	cfg.Reset()

	if cfg.Err() != nil {
		t.Errorf("Err = %v after Reset", cfg.Err())
	}
	if len(cfg.PoseidonOptions()) != 0 {
		t.Errorf("Pass has %d entries after Reset", len(cfg.PoseidonOptions()))
	}
	if cfg.Codec() != nil {
		t.Error("Codec survived Reset")
	}
}

// TestPoseidonOptionsDoNotAliasAcrossCopies is the pass-through list's version
// of the metadata rule. It has the same shape and the same consequence: two
// copies appending into one array give one call another's option.
func TestPoseidonOptionsDoNotAliasAcrossCopies(t *testing.T) {
	// Three, for the reason TestValueCopyDoesNotAlias gives: one option leaves
	// no spare capacity, so the copies reallocate anyway and the test passes on
	// a broken build.
	var base pgrpc.CallConfig
	base.Apply(
		pgrpc.MaxRecvMessageSize(1<<20),
		pgrpc.MaxRecvMessageSize(1<<20),
		pgrpc.MaxRecvMessageSize(1<<20),
	)
	if cap(base.PoseidonOptions()) <= len(base.PoseidonOptions()) {
		t.Fatalf("the base has no spare capacity (len=%d cap=%d); this test cannot detect aliasing",
			len(base.PoseidonOptions()), cap(base.PoseidonOptions()))
	}

	a, b := base, base
	a.Apply(pgrpc.MaxRecvMessageSize(1 << 21))
	b.Apply(pgrpc.MaxRecvMessageSize(1 << 22))

	if len(a.PoseidonOptions()) != 4 || len(b.PoseidonOptions()) != 4 {
		t.Fatalf("a=%d b=%d, want 4 each", len(a.PoseidonOptions()), len(b.PoseidonOptions()))
	}
	if &a.PoseidonOptions()[3] == &b.PoseidonOptions()[3] {
		t.Error("two copies share one option array")
	}
	if len(base.PoseidonOptions()) != 3 {
		t.Errorf("the base grew to %d options", len(base.PoseidonOptions()))
	}
}

func TestPassAccumulates(t *testing.T) {
	var cfg pgrpc.CallConfig
	cfg.Apply(
		pgrpc.MaxRecvMessageSize(1<<20),
		pgrpc.WithPoseidonCallOptions(grpc.MaxRecvMessageSize(1<<21)),
	)
	if len(cfg.PoseidonOptions()) != 2 {
		t.Errorf("Pass has %d entries, want 2 — options must accumulate, not replace",
			len(cfg.PoseidonOptions()))
	}
}

func TestApplyIgnoresNilOptions(t *testing.T) {
	var cfg pgrpc.CallConfig
	cfg.Apply(nil, pgrpc.WithHeaderString("a", "1"), nil)
	if cfg.Err() != nil {
		t.Fatalf("Err = %v", cfg.Err())
	}
	if len(cfg.Metadata()) != 1 {
		t.Errorf("got %d entries, want 1", len(cfg.Metadata()))
	}
}

func TestLaterScalarOptionWins(t *testing.T) {
	first, second := stubCodec{name: "first"}, stubCodec{name: "second"}
	var cfg pgrpc.CallConfig
	cfg.Apply(pgrpc.WithCallCodec(first), pgrpc.WithCallCodec(second))
	if cfg.Codec().Name() != "second" {
		t.Errorf("Codec = %q, want the later option to win", cfg.Codec().Name())
	}
}

// TestAppendFieldIsUsableFromAnOutsideOption keeps the "genuinely OPEN
// interface" claim true. With the fields unexported, an option written outside
// this package needs an exported mutator or it can do nothing at all.
func TestAppendFieldIsUsableFromAnOutsideOption(t *testing.T) {
	shared := mdOf(t, "x-tenant", "acme")
	var cfg pgrpc.CallConfig
	cfg.SetMetadata(shared)
	cfg.Apply(outsideOption{})

	if cfg.Err() != nil {
		t.Fatalf("Err = %v", cfg.Err())
	}
	if got := names(cfg.Metadata()); len(got) != 2 || got[1] != "x-outside" {
		t.Errorf("metadata = %v", got)
	}
	if sameArray(cfg.Metadata(), shared) {
		t.Error("AppendField wrote into a borrowed array")
	}
}

// outsideOption is what a user's own CallOption looks like.
type outsideOption struct{}

func (outsideOption) Apply(c *pgrpc.CallConfig) {
	c.AppendField(conn.HeaderField{Name: []byte("x-outside"), Value: []byte("1")})
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
