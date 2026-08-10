// A runnable load generator, and the repository's proof of what a USER's binary
// links.
//
// This is the only module here that is shaped like a consumer: it imports the
// generated client and nothing else from this repo. `go list -deps` on it must
// show poseidon, pgrpc and protobuf — and must NOT show
// google.golang.org/protobuf/compiler/protogen, which only the plugin needs, or
// grpc-go, which is the thing being avoided. CI checks exactly that.
//
// It reuses testdata's schema rather than carrying its own, so the example can
// never drift from what the generator actually produces.
module github.com/lodgvideon/protoc-gen-go-poseidon/examples/loadgen

go 1.25.0

require (
	github.com/lodgvideon/poseidon-http-client v0.12.0
	github.com/lodgvideon/protoc-gen-go-poseidon/pgrpc v0.0.0
	github.com/lodgvideon/protoc-gen-go-poseidon/testdata v0.0.0
)

require google.golang.org/protobuf v1.36.11 // indirect

replace (
	github.com/lodgvideon/protoc-gen-go-poseidon/pgrpc => ../../pgrpc
	github.com/lodgvideon/protoc-gen-go-poseidon/testdata => ../../testdata
)
