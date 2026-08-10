// Package version carries the plugin's version, in one place.
//
// Two things read it. The plugin prints it for `protoc-gen-go-poseidon
// --version`, matching what protoc-gen-go and protoc-gen-go-grpc do, so a bug
// report can say which build produced a file. And the generator stamps it into
// every generated file's header comment, so the same question can be answered
// from the output alone, long after the binary that wrote it is gone.
package version

// Version is the plugin's semantic version, without a leading "v".
//
// A -dev suffix means the build is between releases: generated output from it
// is not covered by any compatibility promise.
const Version = "0.1.0"

// String returns the version in the form the --version flag prints, matching
// protoc-gen-go's "protoc-gen-go v1.36.11" convention.
func String() string { return "protoc-gen-go-poseidon v" + Version }
