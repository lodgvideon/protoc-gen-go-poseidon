// This module exists for ONE reason: to compile the generator's output.
//
// Go's toolchain skips any directory named testdata, so `go build ./...` from
// the repo root never reaches these files. A module whose ROOT is testdata
// walks normally, because the walk starts below the offending name — so the CI
// matrix gains a row rather than the repo gaining a differently-named fixture
// directory.
//
// It is deliberately NOT part of the root module. The plugin binary must
// require nothing but protobuf; a fixture in the root module would drag
// poseidon and the pgrpc runtime into the plugin's own dependency set.
//
// The replace points at the in-tree runtime, so a breaking change to pgrpc
// fails here rather than after release.
module github.com/lodgvideon/protoc-gen-go-poseidon/testdata

go 1.25.0

require (
	github.com/lodgvideon/protoc-gen-go-poseidon/pgrpc v0.0.0
	google.golang.org/protobuf v1.36.11
)

require github.com/lodgvideon/poseidon-http-client v0.12.0 // indirect

replace github.com/lodgvideon/protoc-gen-go-poseidon/pgrpc => ../pgrpc
