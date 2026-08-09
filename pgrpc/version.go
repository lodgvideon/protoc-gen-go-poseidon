package pgrpc

// SupportPackageIsVersion1 is referenced by every generated file, and exists so
// that regenerating with a newer plugin against an older pgrpc fails at compile
// time instead of at run time.
//
// The mechanism is the one protoc-gen-go-grpc uses. Generated code contains:
//
//	const _ = pgrpc.SupportPackageIsVersion1
//
// If a future plugin emits code this package cannot support, that release drops
// SupportPackageIsVersion1 and introduces SupportPackageIsVersion2. A stale
// pgrpc then fails to build with "undefined: pgrpc.SupportPackageIsVersion2",
// which names the actual problem, rather than failing somewhere inside a
// generated call with a mismatched signature.
//
// It is a bool rather than an int for the same reason it is in grpc-go: the
// value is never read, only its existence is, and a bool cannot be mistaken for
// something worth comparing.
const SupportPackageIsVersion1 = true
