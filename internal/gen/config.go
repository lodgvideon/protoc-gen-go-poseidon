// Package gen implements protoc-gen-go-poseidon's code generation.
package gen

import (
	"flag"
	"fmt"
	"go/token"
	"strconv"

	"google.golang.org/protobuf/compiler/protogen"
)

// DefaultRuntimeImport is the import path of the runtime the generated code
// calls into.
const DefaultRuntimeImport = "github.com/lodgvideon/protoc-gen-go-poseidon/pgrpc"

// DefaultPackageSuffix is the sub-directory and package name generated files go
// into.
const DefaultPackageSuffix = "poseidon"

// Codec names for the default_codec option.
const (
	// CodecProtoCodec makes the generated constructors able to supply
	// protocodec.Codec{} themselves.
	CodecProtoCodec = "protocodec"
	// CodecNone emits no default, so a caller must configure one.
	CodecNone = "none"
)

// Config is the resolved plugin configuration.
type Config struct {
	// SeparatePackage puts generated code in a sub-package of the message
	// package. It is the only supported mode; see Validate.
	SeparatePackage bool
	// PackageSuffix names that sub-package and its directory.
	PackageSuffix string
	// Callers emits the buffer-reusing face.
	Callers bool
	// Interfaces emits the client interface. The implementation and its
	// constructor are emitted either way.
	Interfaces bool
	// MethodConsts emits the full-method-name constants.
	MethodConsts bool
	// RuntimeImport redirects every runtime reference, for a fork or a vendored
	// path.
	RuntimeImport string
	// DefaultCodec selects whether the extra constructors that supply a codec
	// are emitted.
	DefaultCodec string

	// PluginVersion is stamped into every generated file's header. It is set by
	// the command, not by an option.
	PluginVersion string
}

// boolFlag is a flag.Value for an option that may be written bare.
//
// protogen splits each --go-poseidon_opt entry on the first "=" and passes ""
// as the value when there is none, so a bare `callers` reaches Set with an
// empty string. flag.FlagSet.Bool would hand that to strconv.ParseBool, which
// rejects it — making the natural spelling of a boolean option an error.
type boolFlag bool

func (b *boolFlag) String() string   { return strconv.FormatBool(bool(*b)) }
func (b *boolFlag) IsBoolFlag() bool { return true }

// Set implements flag.Value. An empty value means the option was written bare
// and is therefore true.
func (b *boolFlag) Set(s string) error {
	if s == "" {
		*b = true
		return nil
	}
	v, err := strconv.ParseBool(s)
	*b = boolFlag(v)
	return err
}

// Bind registers the plugin's options on fs and installs their defaults.
//
// fs is a FlagSet that is never Parse()d: protogen drives it through ParamFunc,
// once per option key it does not consume itself. Registering these on
// flag.CommandLine instead would be a bug — that one handles os.Args, which
// protoc never populates.
func (c *Config) Bind(fs *flag.FlagSet) {
	*c = Config{
		SeparatePackage: true,
		PackageSuffix:   DefaultPackageSuffix,
		Callers:         true,
		Interfaces:      true,
		MethodConsts:    true,
		RuntimeImport:   DefaultRuntimeImport,
		DefaultCodec:    CodecProtoCodec,
	}
	fs.Var((*boolFlag)(&c.SeparatePackage), "separate_package",
		"emit into a sub-package of the message package (the only supported mode)")
	fs.Var((*boolFlag)(&c.Callers), "callers", "emit the buffer-reusing XCaller face")
	fs.Var((*boolFlag)(&c.Interfaces), "interfaces", "emit the XClient interface")
	fs.Var((*boolFlag)(&c.MethodConsts), "method_consts", "emit the full-method-name constants")
	fs.StringVar(&c.PackageSuffix, "package_suffix", DefaultPackageSuffix,
		"sub-package name and directory for generated code")
	fs.StringVar(&c.RuntimeImport, "runtime_import", DefaultRuntimeImport,
		"import path of the pgrpc runtime")
	fs.StringVar(&c.DefaultCodec, "default_codec", CodecProtoCodec,
		"protocodec | none: whether to emit constructors that supply a default codec")
}

// Validate checks the resolved configuration before any file is generated.
func (c *Config) Validate() error {
	// The flat mode — generating into the same Go package as the .pb.go files —
	// is not shipped. It has no structural collision argument: Go's
	// GoCamelCase drops "_" only before a lowercase letter, so for any
	// identifier we could choose there is a .proto symbol that produces exactly
	// it, and safety would require enumerating everything protoc-gen-go and
	// protoc-gen-go-grpc can emit for the whole Go package and failing on any
	// intersection. The sub-package default needs none of that: distinct
	// package blocks are disjoint scopes.
	if !c.SeparatePackage {
		return fmt.Errorf("separate_package=false is not supported: generating into the " +
			"message package cannot be proven collision-free, and the default " +
			"separate_package=true is immune by construction")
	}
	if err := validIdent("package_suffix", c.PackageSuffix); err != nil {
		return err
	}
	if c.RuntimeImport == "" {
		return fmt.Errorf("runtime_import must not be empty")
	}
	switch c.DefaultCodec {
	case CodecProtoCodec, CodecNone:
	default:
		return fmt.Errorf("default_codec=%q: want %q or %q", c.DefaultCodec, CodecProtoCodec, CodecNone)
	}
	return nil
}

// validIdent reports whether s can be used as a Go package name.
func validIdent(option, s string) error {
	if s == "" {
		return fmt.Errorf("%s must not be empty", option)
	}
	// The keyword check comes FIRST and is not redundant with the next one:
	// token.IsIdentifier already returns false for a keyword, so checking it
	// second would be dead code and the user would be told "range" is not a
	// valid identifier — true, but not the reason.
	if token.Lookup(s).IsKeyword() {
		return fmt.Errorf("%s=%q is a Go keyword and cannot be a package name", option, s)
	}
	if !token.IsIdentifier(s) {
		return fmt.Errorf("%s=%q is not a valid Go identifier", option, s)
	}
	return nil
}

// runtimePackages are the import paths the generated code may reference.
type runtimePackages struct {
	ctx        protogen.GoImportPath
	pgrpc      protogen.GoImportPath
	protocodec protogen.GoImportPath
}

func (c *Config) packages() runtimePackages {
	return runtimePackages{
		ctx:        protogen.GoImportPath("context"),
		pgrpc:      protogen.GoImportPath(c.RuntimeImport),
		protocodec: protogen.GoImportPath(c.RuntimeImport + "/protocodec"),
	}
}

// emitDefaultCodec reports whether the codec-supplying constructors are wanted.
func (c *Config) emitDefaultCodec() bool { return c.DefaultCodec == CodecProtoCodec }
