package pgrpc

import (
	"github.com/lodgvideon/poseidon-http-client/conn"
	"github.com/lodgvideon/poseidon-http-client/grpc"
)

// Re-exports of poseidon's types, so that generated code and its callers can
// name a status code or build a metadata field while importing only this
// package. poseidon's own conn/aliases.go does the same thing for frame and
// hpack, for the same reason.
//
// Every one of these is a true alias, not a defined type. That matters more
// here than it does one layer down: a caller who does reach for poseidon
// directly — to open the connection, which is the normal case — must be able to
// pass a grpc.CallOption or a []conn.HeaderField straight in, and an
// errors.As(err, &pgrpc.Status{}) has to match a status produced inside
// poseidon. A defined type would break both, silently and only at the seams.

type (
	// Status is an RPC's terminal status: a code and a message.
	Status = grpc.Status

	// Code is a gRPC status code.
	Code = grpc.Code

	// HeaderField is one metadata entry, a name/value pair of bytes plus its
	// HPACK indexing mode.
	HeaderField = conn.HeaderField

	// IndexingMode selects a header field's HPACK literal representation, and
	// therefore whether it enters the connection's dynamic table.
	IndexingMode = conn.IndexingMode
)

// Deliberately NOT aliased here: poseidon's grpc.CallOption. This package
// declares its own CallOption, because poseidon's is a closed interface — its
// apply method is unexported — and a generated client needs to carry things
// poseidon has no constructor for, starting with a per-call codec. The two are
// bridged by WithPoseidonCallOptions rather than by an alias, so that the name
// CallOption in generated code always means this package's.

// The gRPC status codes, re-exported so a caller comparing against one does not
// have to import poseidon's grpc package for the constant alone.
const (
	OK                 = grpc.OK
	Canceled           = grpc.Canceled
	Unknown            = grpc.Unknown
	InvalidArgument    = grpc.InvalidArgument
	DeadlineExceeded   = grpc.DeadlineExceeded
	NotFound           = grpc.NotFound
	AlreadyExists      = grpc.AlreadyExists
	PermissionDenied   = grpc.PermissionDenied
	ResourceExhausted  = grpc.ResourceExhausted
	FailedPrecondition = grpc.FailedPrecondition
	Aborted            = grpc.Aborted
	OutOfRange         = grpc.OutOfRange
	Unimplemented      = grpc.Unimplemented
	Internal           = grpc.Internal
	Unavailable        = grpc.Unavailable
	DataLoss           = grpc.DataLoss
	Unauthenticated    = grpc.Unauthenticated
)

// The HPACK indexing modes. IndexNever is the one that carries a security
// meaning: it keeps a credential out of the connection's dynamic table, where
// it would otherwise outlive the RPC that carried it and be emitted as a
// one-byte index on every later call.
const (
	IndexIncremental = conn.IndexIncremental
	IndexWithout     = conn.IndexWithout
	IndexNever       = conn.IndexNever
)
