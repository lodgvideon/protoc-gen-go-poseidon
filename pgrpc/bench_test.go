package pgrpc_test

import (
	"context"
	"testing"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/lodgvideon/protoc-gen-go-poseidon/pgrpc"
	"github.com/lodgvideon/protoc-gen-go-poseidon/pgrpc/protocodec"
)

// Read allocs/op from these, not ns/op.
//
// There is no socket and no server here, on purpose. A benchmark that dials an
// in-process gRPC server charges that server's allocations to the same counter,
// which turns "what does this layer cost" into "what does this layer plus a
// server cost" — a number that cannot be acted on. What is measured here is
// exactly the code between a caller and poseidon's Invoke.
//
// The pairs are the point. Each shape is run twice, once with a codec that does
// nothing and once with the real protobuf codec, so the reader can see which
// allocations belong to this package and which belong to protobuf. Optimising
// the second set from here is not possible; optimising the first is.

// nopCodec is a codec that allocates nothing, so a benchmark using it measures
// this package alone.
//
// It is not a strawman: a caller with pre-marshalled fixtures — the load
// generator this whole module is aimed at — genuinely has a codec this cheap.
type nopCodec struct{ wire []byte }

func (c nopCodec) MarshalAppend(dst []byte, _ any) ([]byte, error) {
	return append(dst, c.wire...), nil
}
func (c nopCodec) Unmarshal(_ []byte, _ any) error { return nil }
func (nopCodec) Name() string                      { return "nop" }

// benchFixture builds a client whose transport does no I/O.
func benchFixture(b *testing.B, cd pgrpc.Codec) (*pgrpc.Client, *wrapperspb.StringValue, *wrapperspb.StringValue) {
	b.Helper()
	resp, err := protocodec.Codec{}.MarshalAppend(nil, wrapperspb.String("pong"))
	if err != nil {
		b.Fatalf("fixture: %v", err)
	}
	f := &fakeInvoker{resp: resp}
	// Pre-grow the fake's request buffer so the first call's growth does not
	// land in the measured loop.
	f.req = make([]byte, 0, 256)
	return pgrpc.NewClient(f, pgrpc.WithCodec(cd)), wrapperspb.String("ping"), &wrapperspb.StringValue{}
}

func benchUnaryOpts(b *testing.B, cd pgrpc.Codec) {
	c, in, out := benchFixture(b, cd)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := pgrpc.UnaryOpts(ctx, c, testMethod, in, out); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkUnaryOpts_Nop is the ergonomic face with the codec cost removed.
//
// It cannot reach zero: &cfg escapes through the interface call in Apply, so
// every call allocates one CallConfig even with no options at all. That is the
// allocation the resolved-config path below exists to avoid, and it is why the
// generated Caller face exists.
func BenchmarkUnaryOpts_Nop(b *testing.B) { benchUnaryOpts(b, nopCodec{wire: []byte("ping")}) }

// BenchmarkUnaryOpts_Proto is the same path a generated XClient method takes.
func BenchmarkUnaryOpts_Proto(b *testing.B) { benchUnaryOpts(b, protocodec.Codec{}) }

func benchUnaryResolved(b *testing.B, cd pgrpc.Codec) {
	c, in, out := benchFixture(b, cd)
	ctx := context.Background()
	cfg := &pgrpc.CallConfig{}
	var reqBuf, respBuf []byte

	// Warm up outside the measured loop: the first call allocates both scratch
	// buffers, and counting that once across N iterations would understate the
	// steady state by a rounding error and overstate it at low N.
	for range 2 {
		if err := pgrpc.Unary(ctx, c, cfg, testMethod, in, out, &reqBuf, &respBuf); err != nil {
			b.Fatal(err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := pgrpc.Unary(ctx, c, cfg, testMethod, in, out, &reqBuf, &respBuf); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkUnaryResolved_Nop is the number this module is built around: a
// resolved configuration, caller-owned buffers, and a codec that does not
// allocate. Anything above zero here is this package's own overhead.
func BenchmarkUnaryResolved_Nop(b *testing.B) { benchUnaryResolved(b, nopCodec{wire: []byte("ping")}) }

// BenchmarkUnaryResolved_Proto is what a generated XCaller method costs today.
// The difference from the Nop pair is protobuf's, not ours.
func BenchmarkUnaryResolved_Proto(b *testing.B) { benchUnaryResolved(b, protocodec.Codec{}) }

// BenchmarkOptionResolution isolates the ergonomic path's per-call
// configuration work from the RPC entirely, so the CallConfig cost is visible
// as its own line rather than inferred from a difference.
func BenchmarkOptionResolution(b *testing.B) {
	md := mdOf(b, "x-tenant", "acme")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		var cfg pgrpc.CallConfig
		cfg.Apply(pgrpc.WithMetadata(md))
		if cfg.Err != nil {
			b.Fatal(cfg.Err)
		}
	}
}

// BenchmarkMetadataBuilder measures the steady state the builder exists for:
// metadata whose VALUES change per RPC while its keys do not.
func BenchmarkMetadataBuilder(b *testing.B) {
	var m pgrpc.Metadata
	ids := [][]byte{[]byte("r-1"), []byte("r-2"), []byte("r-3")}
	// Warm up: the first Set of each key interns its lowercased name and the
	// arena grows to its high-water mark. Those are one-time costs and belong
	// outside the loop, which is the only place the builder's claim holds.
	for _, id := range ids {
		m.Reset()
		if err := m.SetText("x-request-id", id); err != nil {
			b.Fatal(err)
		}
		if err := m.SetBin("x-trace-bin", id); err != nil {
			b.Fatal(err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	i := 0
	for b.Loop() {
		m.Reset()
		id := ids[i%len(ids)]
		i++
		if err := m.SetText("x-request-id", id); err != nil {
			b.Fatal(err)
		}
		if err := m.SetBin("x-trace-bin", id); err != nil {
			b.Fatal(err)
		}
		_ = m.Fields()
	}
}

// BenchmarkMetadataAppendMetadata is the same work through poseidon's own
// builder, which is what the Metadata type is claiming to beat. Without this
// line the claim is unfalsifiable.
func BenchmarkMetadataAppendMetadata(b *testing.B) {
	ids := [][]byte{[]byte("r-1"), []byte("r-2"), []byte("r-3")}
	b.ReportAllocs()
	b.ResetTimer()
	i := 0
	for b.Loop() {
		var cfg pgrpc.CallConfig
		id := ids[i%len(ids)]
		i++
		cfg.Apply(
			pgrpc.WithHeader("x-request-id", id),
			pgrpc.WithHeader("x-trace-bin", id),
		)
		if cfg.Err != nil {
			b.Fatal(cfg.Err)
		}
	}
}

// BenchmarkUnaryResolved_NilScratch_Nop isolates what passing nil buffers
// costs, so the ergonomic path's total can be DECOMPOSED rather than asserted.
// The difference from BenchmarkUnaryResolved_Nop is exactly the two escaping
// slice headers Unary allocates when a caller supplies none.
func BenchmarkUnaryResolved_NilScratch_Nop(b *testing.B) {
	c, in, out := benchFixture(b, nopCodec{wire: []byte("ping")})
	ctx := context.Background()
	cfg := &pgrpc.CallConfig{}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := pgrpc.Unary(ctx, c, cfg, testMethod, in, out, nil, nil); err != nil {
			b.Fatal(err)
		}
	}
}
