package vtcodec_test

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/lodgvideon/protoc-gen-go-poseidon/pgrpc/vtcodec"
)

// payload is the body every fake message marshals. Its exact bytes do not
// matter; that it lands at the right OFFSET does.
var payload = []byte{0xDE, 0xAD, 0xBE, 0xEF}

// fakeVT is a message with the canonical vtprotobuf method set:
// features=marshal+unmarshal+size, no pool, no strict. It has Reset (every
// protoc-gen-go message does) but no ResetVT.
type fakeVT struct {
	size    int
	written int // what MarshalToSizedBufferVT reports, defaults to size
	err     error
	got     []byte
	resets  int
}

func (m *fakeVT) SizeVT() int { return m.size }

// MarshalToSizedBufferVT fills BACKWARDS from the end, as vtprotobuf's real one
// does. Reproducing that here is the point of the fake: a fill that wrote from
// the front would make the short-write test pass for the wrong reason.
func (m *fakeVT) MarshalToSizedBufferVT(dst []byte) (int, error) {
	if m.err != nil {
		return 0, m.err
	}
	n := m.written
	if n == 0 {
		n = m.size
	}
	copy(dst[len(dst)-n:], payload[:min(n, len(payload))])
	return n, nil
}

func (m *fakeVT) UnmarshalVT(b []byte) error { m.got = append(m.got, b...); return nil }
func (m *fakeVT) Reset()                     { m.resets++; m.got = nil }

// fakeStrict carries the method set vtprotobuf's `strict` feature emits. A
// probe that only knew the non-strict name would route this to the fallback and
// nobody would notice, because the fallback produces correct bytes.
type fakeStrict struct{ size int }

func (m *fakeStrict) SizeVT() int { return m.size }
func (m *fakeStrict) MarshalToSizedBufferVTStrict(dst []byte) (int, error) {
	copy(dst[len(dst)-m.size:], payload[:min(m.size, len(payload))])
	return m.size, nil
}

// fakePooled has ResetVT as well, which vtprotobuf emits only for messages
// opted into the pool feature. ResetVT must win over Reset.
type fakePooled struct {
	fakeVT
	vtResets int
}

func (m *fakePooled) ResetVT() { m.vtResets++ }

// fakeNoReset parses but cannot be reset. Since UnmarshalVT is merge-shaped,
// reusing it would silently accumulate — so it must be refused, not tolerated.
type fakeNoReset struct{ got []byte }

func (m *fakeNoReset) UnmarshalVT(b []byte) error { m.got = append(m.got, b...); return nil }

func TestMarshalAppendUsesVTAndPreservesPrefix(t *testing.T) {
	var c vtcodec.Codec
	prefix := []byte("KEEPME")
	dst := append([]byte(nil), prefix...)

	out, err := c.MarshalAppend(dst, &fakeVT{size: 4})
	if err != nil {
		t.Fatalf("MarshalAppend: %v", err)
	}
	if got := string(out[:len(prefix)]); got != string(prefix) {
		t.Errorf("prefix clobbered: got %q, want %q", got, prefix)
	}
	if got, want := len(out), len(prefix)+4; got != want {
		t.Fatalf("len(out) = %d, want %d", got, want)
	}
	if got := out[len(prefix):]; string(got) != string(payload) {
		t.Errorf("payload = % x, want % x", got, payload)
	}
}

func TestMarshalAppendUsesStrictMethodSet(t *testing.T) {
	var c vtcodec.Codec
	out, err := c.MarshalAppend(nil, &fakeStrict{size: 4})
	if err != nil {
		t.Fatalf("MarshalAppend: %v", err)
	}
	if string(out) != string(payload) {
		t.Errorf("payload = % x, want % x", out, payload)
	}
}

// TestMarshalAppendRejectsShortWrite is the one that matters. A short write
// leaves the bytes at the END of the sized buffer, so slicing to the reported
// length would return uninitialised memory that the framing layer then stamps a
// valid length prefix over — the server decodes junk and nothing errors.
func TestMarshalAppendRejectsShortWrite(t *testing.T) {
	var c vtcodec.Codec
	_, err := c.MarshalAppend(nil, &fakeVT{size: 8, written: 4})
	if err == nil {
		t.Fatal("short write accepted; it must be rejected, not mis-sliced")
	}
	if !strings.Contains(err.Error(), "wrote 4 of 8") {
		t.Errorf("error does not name the mismatch: %v", err)
	}
}

func TestMarshalAppendPropagatesMarshalError(t *testing.T) {
	var c vtcodec.Codec
	sentinel := errors.New("boom")
	if _, err := c.MarshalAppend(nil, &fakeVT{size: 4, err: sentinel}); !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want %v", err, sentinel)
	}
}

func TestUnmarshalPrefersResetVTOverReset(t *testing.T) {
	var c vtcodec.Codec
	m := &fakePooled{}
	if err := c.Unmarshal(payload, m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if m.vtResets != 1 {
		t.Errorf("ResetVT called %d times, want 1", m.vtResets)
	}
	if m.resets != 0 {
		t.Errorf("Reset called %d times, want 0 — ResetVT must win", m.resets)
	}
}

// TestUnmarshalResetsBetweenCalls guards the merge-shape trap directly: without
// a reset, a reused message accumulates every prior response. That only ever
// shows up under reuse, which is exactly the path this package exists for.
func TestUnmarshalResetsBetweenCalls(t *testing.T) {
	var c vtcodec.Codec
	m := &fakeVT{}
	for i := range 3 {
		if err := c.Unmarshal(payload, m); err != nil {
			t.Fatalf("Unmarshal #%d: %v", i, err)
		}
	}
	if m.resets != 3 {
		t.Errorf("Reset called %d times, want 3", m.resets)
	}
	if len(m.got) != len(payload) {
		t.Errorf("accumulated %d bytes across 3 calls, want %d — the reset did not happen",
			len(m.got), len(payload))
	}
}

func TestUnmarshalRefusesMessageWithNoReset(t *testing.T) {
	var c vtcodec.Codec
	err := c.Unmarshal(payload, &fakeNoReset{})
	if err == nil {
		t.Fatal("a message with UnmarshalVT but no reset was accepted")
	}
	if !strings.Contains(err.Error(), "merge-shaped") {
		t.Errorf("error does not explain why: %v", err)
	}
}

// TestFallbackHandlesPlainProtoMessage covers the partly-vt-generated schema:
// wrapperspb has none of vtprotobuf's methods, so both directions must route to
// protocodec and still round-trip.
func TestFallbackHandlesPlainProtoMessage(t *testing.T) {
	var c vtcodec.Codec
	in := wrapperspb.String("hello")

	wire, err := c.MarshalAppend(nil, in)
	if err != nil {
		t.Fatalf("MarshalAppend: %v", err)
	}
	want, err := proto.Marshal(in)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}
	if string(wire) != string(want) {
		t.Errorf("fallback wire bytes = % x, want % x", wire, want)
	}

	out := &wrapperspb.StringValue{}
	if err := c.Unmarshal(wire, out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.GetValue() != "hello" {
		t.Errorf("round trip = %q, want %q", out.GetValue(), "hello")
	}
}

// TestZeroValueCodecDoesNotPanic pins the decision recorded on Codec.Fallback:
// a nil Fallback defaults to protocodec rather than nil-panicking on the first
// RPC.
func TestZeroValueCodecDoesNotPanic(t *testing.T) {
	if _, err := (vtcodec.Codec{}).MarshalAppend(nil, wrapperspb.Int32(7)); err != nil {
		t.Fatalf("zero-value Codec failed on a non-vt message: %v", err)
	}
}

func TestName(t *testing.T) {
	if got := (vtcodec.Codec{}).Name(); got != "proto" {
		t.Errorf("Name() = %q, want %q", got, "proto")
	}
}
