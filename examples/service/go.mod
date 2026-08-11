// The ordinary-service example: an HTTP handler calling a gRPC backend.
//
// Separate from examples/loadgen because the two demonstrate opposite shapes —
// one RPC per request through the ergonomic face here, versus one Caller per
// virtual user with reused buffers there — and a reader should be able to open
// exactly one of them.
module github.com/lodgvideon/protoc-gen-go-poseidon/examples/service

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
