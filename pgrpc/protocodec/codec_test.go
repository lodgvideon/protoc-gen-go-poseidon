package protocodec_test

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/lodgvideon/protoc-gen-go-poseidon/pgrpc"
	"github.com/lodgvideon/protoc-gen-go-poseidon/pgrpc/protocodec"
)

var _ pgrpc.Codec = protocodec.Codec{}

func TestRoundTrip(t *testing.T) {
	var c protocodec.Codec
	wire, err := c.MarshalAppend(nil, wrapperspb.String("hello"))
	if err != nil {
		t.Fatalf("MarshalAppend: %v", err)
	}
	out := &wrapperspb.StringValue{}
	if err := c.Unmarshal(wire, out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.GetValue() != "hello" {
		t.Errorf("round trip = %q, want %q", out.GetValue(), "hello")
	}
}

// TestMarshalAppendPreservesPrefix is the append contract. A MarshalAppend that
// ignored dst and returned a fresh slice would pass every round-trip test in
// this file while costing an allocation per RPC — the exact thing this codec
// shape exists to avoid.
func TestMarshalAppendPreservesPrefix(t *testing.T) {
	var c protocodec.Codec
	prefix := []byte("KEEPME")
	out, err := c.MarshalAppend(append([]byte(nil), prefix...), wrapperspb.String("x"))
	if err != nil {
		t.Fatalf("MarshalAppend: %v", err)
	}
	if !strings.HasPrefix(string(out), string(prefix)) {
		t.Fatalf("prefix lost: got %q", out)
	}
	body := &wrapperspb.StringValue{}
	if err := c.Unmarshal(out[len(prefix):], body); err != nil {
		t.Fatalf("Unmarshal of the appended region: %v", err)
	}
	if body.GetValue() != "x" {
		t.Errorf("appended message = %q, want %q", body.GetValue(), "x")
	}
}

// TestUnmarshalResetsEvenWhenMergeRequested pins the reset-shaped contract
// against the one way a caller can break it from outside: setting
// UnmarshalOpts.Merge. Without the guard, a reused out message accumulates
// every prior response's repeated fields, and only under reuse — which is the
// buffer-reusing path this module is built for.
func TestUnmarshalResetsEvenWhenMergeRequested(t *testing.T) {
	c := protocodec.Codec{}
	c.UnmarshalOpts.Merge = true

	list, err := structpb.NewList([]any{"a"})
	if err != nil {
		t.Fatalf("NewList: %v", err)
	}
	wire, err := proto.Marshal(list)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}

	out := &structpb.ListValue{}
	for i := range 3 {
		if err := c.Unmarshal(wire, out); err != nil {
			t.Fatalf("Unmarshal #%d: %v", i, err)
		}
	}
	if got := len(out.GetValues()); got != 1 {
		t.Errorf("accumulated %d values across 3 unmarshals, want 1 — Merge was honoured", got)
	}
}

func TestNonProtoMessageIsRejected(t *testing.T) {
	var c protocodec.Codec
	type notAMessage struct{ X int }

	if _, err := c.MarshalAppend(nil, notAMessage{}); err == nil {
		t.Error("MarshalAppend accepted a non-proto.Message")
	} else if !strings.Contains(err.Error(), "not a proto.Message") {
		t.Errorf("MarshalAppend error is unclear: %v", err)
	}

	if err := c.Unmarshal(nil, &notAMessage{}); err == nil {
		t.Error("Unmarshal accepted a non-proto.Message")
	} else if !strings.Contains(err.Error(), "not a proto.Message") {
		t.Errorf("Unmarshal error is unclear: %v", err)
	}
}

// TestMarshalAppendReturnsDstOnError keeps the documented failure shape: dst
// comes back so a caller reusing a scratch buffer does not lose it, even though
// its contents past the original length are unspecified.
func TestMarshalAppendReturnsDstOnError(t *testing.T) {
	var c protocodec.Codec
	dst := make([]byte, 4, 64)
	out, err := c.MarshalAppend(dst, "not a message")
	if err == nil {
		t.Fatal("expected an error")
	}
	if cap(out) != cap(dst) {
		t.Errorf("cap(out) = %d, want %d — the caller's buffer was dropped", cap(out), cap(dst))
	}
}

func TestName(t *testing.T) {
	if got := (protocodec.Codec{}).Name(); got != "proto" {
		t.Errorf("Name() = %q, want %q", got, "proto")
	}
}
