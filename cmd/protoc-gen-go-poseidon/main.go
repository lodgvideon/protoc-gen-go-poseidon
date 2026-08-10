// Command protoc-gen-go-poseidon generates typed gRPC clients that call into
// the pgrpc runtime instead of grpc-go.
//
// It is a protoc plugin and is normally invoked by protoc or buf, not directly:
//
//	protoc --go_out=. --go-poseidon_out=. helloworld.proto
//
// Options are passed as --go-poseidon_opt=key=value. See the gen package for
// the list.
package main

import (
	"flag"
	"fmt"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"

	"github.com/lodgvideon/protoc-gen-go-poseidon/internal/gen"
	"github.com/lodgvideon/protoc-gen-go-poseidon/internal/version"
)

func main() {
	// flag.CommandLine handles os.Args, which protoc never populates. It exists
	// only for a human typing --version, and this must return BEFORE Run:
	// protogen hard-fails on any argv at all, with "unknown argument %q (this
	// program should be run by protoc, not directly)".
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.String())
		return
	}

	// A second, local FlagSet, deliberately never parsed. protogen drives it
	// through ParamFunc, once per --go-poseidon_opt key it does not consume
	// itself (module, paths, annotate_code, M<file>=, default_api_level).
	var cfg gen.Config
	var flags flag.FlagSet
	cfg.Bind(&flags)

	protogen.Options{ParamFunc: flags.Set}.Run(func(p *protogen.Plugin) error {
		// Both feature bits are mandatory, and for the same reason: protoc
		// refuses to run a plugin that declares less than the file needs, so a
		// missing bit means protoc-gen-go succeeds while this one refuses and
		// the user is left with a half-generated package. Without
		// PROTO3_OPTIONAL that happens for any proto3 file using `optional`;
		// without SUPPORTS_EDITIONS, for any file with edition >= 2023.
		p.SupportedFeatures = uint64(pluginpb.CodeGeneratorResponse_FEATURE_PROTO3_OPTIONAL) |
			uint64(pluginpb.CodeGeneratorResponse_FEATURE_SUPPORTS_EDITIONS)

		// Both bounds, or protogen sends neither — it emits the pair only when
		// both are non-zero. Declaring the editions feature bit while leaving
		// the bounds at zero makes protoc reject every editions file against a
		// maximum of 0.
		//
		// The maximum tracks the PINNED protobuf version: protoc-gen-go
		// declares internal/editionssupport.Maximum, which is EDITION_2024 at
		// v1.36.11. Declaring less than protoc-gen-go is the half-generated
		// package again, triggered by an editions-2024 file instead of a
		// feature bit. Bumping the protobuf pin means revisiting this line.
		p.SupportedEditionsMinimum = descriptorpb.Edition_EDITION_PROTO2
		p.SupportedEditionsMaximum = descriptorpb.Edition_EDITION_2024

		cfg.PluginVersion = version.Version
		return gen.Run(p, cfg)
	})
}
