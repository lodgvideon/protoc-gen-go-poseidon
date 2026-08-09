package pgrpc_test

import (
	"errors"
	"testing"

	"github.com/lodgvideon/poseidon-http-client/conn"
	"github.com/lodgvideon/poseidon-http-client/grpc"
	"github.com/lodgvideon/protoc-gen-go-poseidon/pgrpc"
)

// These fail to COMPILE if any alias is ever turned into a defined type. That
// is the whole contract: a caller holding a poseidon value must be able to pass
// it here and back without a conversion.
var (
	_ grpc.Status       = pgrpc.Status{}
	_ pgrpc.Status      = grpc.Status{}
	_ conn.HeaderField  = pgrpc.HeaderField{}
	_ pgrpc.HeaderField = conn.HeaderField{}
	_ pgrpc.Code        = grpc.Internal
	_ grpc.Code         = pgrpc.Internal
)

// TestStatusAliasSurvivesErrorsAs is the alias contract at its sharpest. A
// status originates inside poseidon; a caller of generated code will reach for
// errors.As with the name this package exports. If Status ever stops being an
// alias, that match silently starts failing and every RPC error degrades to an
// opaque one — no compile error anywhere.
func TestStatusAliasSurvivesErrorsAs(t *testing.T) {
	err := grpc.Status{Code: grpc.Unavailable, Message: "boom"}.Err()
	if err == nil {
		t.Fatal("Err() on a non-OK status returned nil")
	}

	var target *pgrpc.Status
	if !errors.As(err, &target) {
		t.Fatalf("errors.As(%T) did not match a *pgrpc.Status target", err)
	}
	if target.Code != pgrpc.Unavailable {
		t.Errorf("Code = %v, want Unavailable", target.Code)
	}
	if target.Message != "boom" {
		t.Errorf("Message = %q, want %q", target.Message, "boom")
	}
}

// TestCodeAliasesMatchPoseidon is dull on purpose. Seventeen constants copied
// by hand is exactly the shape that acquires a transposed pair — Aborted bound
// to OutOfRange, say — which no compiler catches, because both sides are the
// same type, and which surfaces as a caller retrying on the wrong code.
func TestCodeAliasesMatchPoseidon(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  pgrpc.Code
		want grpc.Code
	}{
		{"OK", pgrpc.OK, grpc.OK},
		{"Canceled", pgrpc.Canceled, grpc.Canceled},
		{"Unknown", pgrpc.Unknown, grpc.Unknown},
		{"InvalidArgument", pgrpc.InvalidArgument, grpc.InvalidArgument},
		{"DeadlineExceeded", pgrpc.DeadlineExceeded, grpc.DeadlineExceeded},
		{"NotFound", pgrpc.NotFound, grpc.NotFound},
		{"AlreadyExists", pgrpc.AlreadyExists, grpc.AlreadyExists},
		{"PermissionDenied", pgrpc.PermissionDenied, grpc.PermissionDenied},
		{"ResourceExhausted", pgrpc.ResourceExhausted, grpc.ResourceExhausted},
		{"FailedPrecondition", pgrpc.FailedPrecondition, grpc.FailedPrecondition},
		{"Aborted", pgrpc.Aborted, grpc.Aborted},
		{"OutOfRange", pgrpc.OutOfRange, grpc.OutOfRange},
		{"Unimplemented", pgrpc.Unimplemented, grpc.Unimplemented},
		{"Internal", pgrpc.Internal, grpc.Internal},
		{"Unavailable", pgrpc.Unavailable, grpc.Unavailable},
		{"DataLoss", pgrpc.DataLoss, grpc.DataLoss},
		{"Unauthenticated", pgrpc.Unauthenticated, grpc.Unauthenticated},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}

// TestCodeAliasesAreDistinct catches the other half of the paste bug: a pair
// bound to the same poseidon constant passes the table above (both sides move
// together) but collapses two codes into one.
func TestCodeAliasesAreDistinct(t *testing.T) {
	codes := map[pgrpc.Code]string{}
	for name, c := range map[string]pgrpc.Code{
		"OK": pgrpc.OK, "Canceled": pgrpc.Canceled, "Unknown": pgrpc.Unknown,
		"InvalidArgument": pgrpc.InvalidArgument, "DeadlineExceeded": pgrpc.DeadlineExceeded,
		"NotFound": pgrpc.NotFound, "AlreadyExists": pgrpc.AlreadyExists,
		"PermissionDenied": pgrpc.PermissionDenied, "ResourceExhausted": pgrpc.ResourceExhausted,
		"FailedPrecondition": pgrpc.FailedPrecondition, "Aborted": pgrpc.Aborted,
		"OutOfRange": pgrpc.OutOfRange, "Unimplemented": pgrpc.Unimplemented,
		"Internal": pgrpc.Internal, "Unavailable": pgrpc.Unavailable,
		"DataLoss": pgrpc.DataLoss, "Unauthenticated": pgrpc.Unauthenticated,
	} {
		if prev, dup := codes[c]; dup {
			t.Errorf("%s and %s are both %d", prev, name, c)
		}
		codes[c] = name
	}
	if len(codes) != 17 {
		t.Errorf("got %d distinct codes, want 17", len(codes))
	}
}

// TestIndexingModeAliases guards the constant whose value has a security
// meaning: IndexNever is what keeps a credential out of the connection's HPACK
// dynamic table. Bound to the wrong mode, every bearer token this package sends
// gets indexed once and then referenced by a one-byte index for the life of the
// connection — with nothing failing anywhere.
func TestIndexingModeAliases(t *testing.T) {
	if pgrpc.IndexIncremental != conn.IndexIncremental {
		t.Error("IndexIncremental does not match conn's")
	}
	if pgrpc.IndexWithout != conn.IndexWithout {
		t.Error("IndexWithout does not match conn's")
	}
	if pgrpc.IndexNever != conn.IndexNever {
		t.Error("IndexNever does not match conn's")
	}
	if pgrpc.IndexNever == pgrpc.IndexIncremental || pgrpc.IndexNever == pgrpc.IndexWithout {
		t.Error("IndexNever is not distinct from the non-sensitive modes")
	}
}
