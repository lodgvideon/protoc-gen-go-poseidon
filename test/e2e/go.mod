// The end-to-end module: the generated client driven against a real grpc-go
// server over a real socket.
//
// It is separate from testdata for one reason. testdata's job is to prove that
// generated output compiles against pgrpc and protobuf and NOTHING ELSE — a
// grpc-go dependency there would quietly destroy that proof. Here grpc-go is
// the point: it is the other implementation, and agreeing with it is the only
// evidence the wire behaviour is right.
module github.com/lodgvideon/protoc-gen-go-poseidon/test/e2e

go 1.25.0

require (
	github.com/lodgvideon/poseidon-http-client v0.12.0
	github.com/lodgvideon/protoc-gen-go-poseidon/pgrpc v0.0.0
	github.com/lodgvideon/protoc-gen-go-poseidon/testdata v0.0.0
	google.golang.org/grpc v1.76.0
)

require (
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20250804133106-a7a43d27e69b // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

replace (
	github.com/lodgvideon/protoc-gen-go-poseidon/pgrpc => ../../pgrpc
	github.com/lodgvideon/protoc-gen-go-poseidon/testdata => ../../testdata
)
