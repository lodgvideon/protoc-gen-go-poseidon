package gen

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

var update = flag.Bool("update", false, "rewrite the golden files from the current output")

const (
	descriptorSet   = "../../testdata/descriptors/all.binpb"
	goldenDir       = "../../testdata/golden"
	helloworldProto = "helloworld/helloworld.proto"
	edgeProto       = "edge/edge.proto"
)

// loadDescriptors reads the checked-in FileDescriptorSet.
//
// It is checked in rather than built at test time so the suite needs neither
// buf nor protoc on PATH, and so a schema change is visible in review as a
// deliberate diff rather than as invisible input drift.
func loadDescriptors(t *testing.T) *descriptorpb.FileDescriptorSet {
	t.Helper()
	raw, err := os.ReadFile(descriptorSet)
	if err != nil {
		t.Fatalf("reading the descriptor set: %v\n"+
			"Rebuild it with: cd testdata && buf build -o descriptors/all.binpb", err)
	}
	var fds descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &fds); err != nil {
		t.Fatalf("parsing the descriptor set: %v", err)
	}
	return &fds
}

// newPlugin builds a protogen.Plugin the way protoc would.
func newPlugin(t *testing.T, generate []string, param string) (*protogen.Plugin, Config) {
	t.Helper()
	fds := loadDescriptors(t)

	// The names MUST match the descriptor set's own, which are always
	// slash-separated. A filepath.Join here would produce backslashes on
	// Windows, match nothing, leave every file Generate=false — and the
	// generator would emit nothing AT ALL, with no error.
	known := make(map[string]bool, len(fds.File))
	for _, f := range fds.File {
		known[f.GetName()] = true
	}
	for _, name := range generate {
		if !known[name] {
			t.Fatalf("%q is not in the descriptor set; it has %d files", name, len(fds.File))
		}
	}

	req := &pluginpb.CodeGeneratorRequest{
		FileToGenerate: generate,
		ProtoFile:      fds.File,
		// CompilerVersion is deliberately nil: that is what buf sends, and the
		// header must render "(unknown)" rather than crash or vary.
	}
	if param != "" {
		req.Parameter = proto.String(param)
	}

	var cfg Config
	var flags flag.FlagSet
	cfg.Bind(&flags)

	p, err := protogen.Options{ParamFunc: flags.Set}.New(req)
	if err != nil {
		t.Fatalf("protogen.New: %v", err)
	}
	p.SupportedFeatures = uint64(pluginpb.CodeGeneratorResponse_FEATURE_PROTO3_OPTIONAL) |
		uint64(pluginpb.CodeGeneratorResponse_FEATURE_SUPPORTS_EDITIONS)
	p.SupportedEditionsMinimum = descriptorpb.Edition_EDITION_PROTO2
	p.SupportedEditionsMaximum = descriptorpb.Edition_EDITION_2024
	// Pinned, not taken from the build, or every release would rewrite every
	// golden file.
	cfg.PluginVersion = "0.0.0-test"

	return p, cfg
}

// runGen generates and returns the single output file's content.
func runGen(t *testing.T, p *protogen.Plugin, cfg Config) (name, content string) {
	t.Helper()
	if err := Run(p, cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Content runs the output through go/parser and go/format, so unparsable
	// output fails HERE, with numbered source, instead of as an unreadable
	// golden diff.
	resp := p.Response()
	if resp.Error != nil {
		t.Fatalf("generation reported: %s", resp.GetError())
	}
	if len(resp.File) != 1 {
		var names []string
		for _, f := range resp.File {
			names = append(names, f.GetName())
		}
		t.Fatalf("got %d generated files %v, want 1", len(resp.File), names)
	}
	return resp.File[0].GetName(), resp.File[0].GetContent()
}

func TestGolden(t *testing.T) {
	for _, tc := range []struct {
		name  string
		param string
	}{
		{"default", ""},
		{"no_callers", "callers=false"},
		{"no_interfaces", "interfaces=false"},
		{"no_method_consts", "method_consts=false"},
		{"codec_none", "default_codec=none"},
		{"suffix_pclient", "package_suffix=pclient"},
		{"bare_bool", "callers"}, // a bare key: protogen delivers "" as the value
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, cfg := newPlugin(t, []string{helloworldProto}, tc.param)
			name, got := runGen(t, p, cfg)

			golden := filepath.Join(goldenDir, tc.name+".golden")
			if *update {
				// Written verbatim. The generator emits "\n" and
				// .gitattributes pins the tree to LF, so no translation here.
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatalf("writing %s: %v", golden, err)
				}
				t.Logf("updated %s (output name %s)", golden, name)
				return
			}

			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("reading %s: %v\nRegenerate with: go test ./internal/gen -run TestGolden -update", golden, err)
			}
			// Normalised on the READ side only, as a net for a file that
			// reached the tree through an editor or a stale core.autocrlf.
			// TestGoldenFilesAreLF is what stops that from being silent.
			if diff := firstDiff(normalizeEOL(string(want)), got); diff != "" {
				t.Errorf("generated output differs from %s:\n%s\n\n"+
					"If the change is intended: go test ./internal/gen -run TestGolden -update", golden, diff)
			}
		})
	}
}

// TestGoldenOutputPath pins where the file lands, which no golden covers: the
// content would be identical if the generator wrote it to the wrong directory.
func TestGoldenOutputPath(t *testing.T) {
	p, cfg := newPlugin(t, []string{helloworldProto}, "")
	name, _ := runGen(t, p, cfg)
	const want = "github.com/lodgvideon/protoc-gen-go-poseidon/testdata/helloworld/poseidon/helloworld_poseidon.pb.go"
	if name != want {
		t.Errorf("output path = %q, want %q", name, want)
	}
	if strings.Contains(name, `\`) {
		t.Error("the output path contains a backslash; response names are always slash-separated")
	}
}

// TestGoldenFilesAreLF fails on any CR in a golden file.
//
// Without it the read-side normalisation above would silently accept a golden
// committed with CRLF, and the next contributor's -update would produce a diff
// touching every line of every file.
func TestGoldenFilesAreLF(t *testing.T) {
	entries, err := os.ReadDir(goldenDir)
	if err != nil {
		t.Fatalf("reading %s: %v", goldenDir, err)
	}
	if len(entries) == 0 {
		t.Fatal("no golden files; run: go test ./internal/gen -run TestGolden -update")
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".golden") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(goldenDir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		if i := bytes.IndexByte(raw, '\r'); i >= 0 {
			t.Errorf("%s contains CR at offset %d; golden files must be LF-only", e.Name(), i)
		}
	}
}

// TestRejectsMethodShadowingTheGuard is the negative case that matters most. A
// method named Enter shadows pgrpc.Guard.Enter on the generated Caller, and the
// generated body's own x.Enter() then recurses forever with no diagnostic
// anywhere. Generation must refuse.
// TestAcceptsMethodsNamedEnterAndLeave pins the un-embedding of pgrpc.Guard.
//
// These names used to be refused, because the Guard was an EMBEDDED field and
// its methods were promoted onto every Caller: a generated method named Enter
// shadowed the promoted one by Go's depth rule, and the body's own x.Enter()
// called itself. Infinite recursion, no compiler diagnostic.
//
// Refusing them was the wrong half of the trade. It meant pgrpc's method set
// leaked into every user's .proto namespace, and every method ever added to
// Guard would retroactively forbid another name. The Guard is a named field
// now, so these are ordinary methods — and the generated body says so.
func TestAcceptsMethodsNamedEnterAndLeave(t *testing.T) {
	p, cfg := newPlugin(t, []string{edgeProto}, "")
	_, content := runGen(t, p, cfg)
	if !strings.Contains(content, "func (x *GuardedCaller) Enter(") {
		t.Error("the generated Caller has no Enter method")
	}
	// The proof the shadow is gone: the guard is reached through the field, so
	// the generated Enter cannot be calling itself.
	if !strings.Contains(content, "x.guard.Enter()") {
		t.Error("the generated body does not go through the guard field")
	}
	// gofmt aligns struct fields, so the field and its type are separated by
	// run-length padding rather than one space.
	if !regexp.MustCompile(`guard\s+pgrpc\.Guard`).MatchString(content) {
		t.Error("pgrpc.Guard is not a named field; embedding it exports Enter and Leave")
	}
}

// TestGuardCollisionIsAllowedWithoutCallers pins that the check is tied to the
// face that actually embeds the Guard. With callers=false nothing is shadowed.
func TestEnterAndLeaveAreFineWithoutCallersToo(t *testing.T) {
	p, cfg := newPlugin(t, []string{edgeProto}, "callers=false")
	if err := Run(p, cfg); err != nil {
		t.Errorf("callers=false rejected a method named Enter: %v", err)
	}
}

func TestSeparatePackageFalseIsRejected(t *testing.T) {
	p, cfg := newPlugin(t, []string{helloworldProto}, "separate_package=false")
	err := Run(p, cfg)
	if err == nil {
		t.Fatal("separate_package=false was accepted")
	}
	if !strings.Contains(err.Error(), "collision-free") {
		t.Errorf("the error does not explain why: %v", err)
	}
}

func TestInvalidOptionsAreRejected(t *testing.T) {
	for _, tc := range []struct{ name, param, want string }{
		{"keyword suffix", "package_suffix=range", "keyword"},
		{"invalid suffix", "package_suffix=not-an-ident", "valid Go identifier"},
		{"empty suffix", "package_suffix=", "empty"},
		{"unknown codec", "default_codec=cbor", "default_codec"},
		{"empty runtime import", "runtime_import=", "runtime_import"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, cfg := newPlugin(t, []string{helloworldProto}, tc.param)
			err := Run(p, cfg)
			if err == nil {
				t.Fatalf("%q was accepted", tc.param)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestUnknownOptionIsRejected covers the ParamFunc path rather than Validate:
// protogen surfaces the FlagSet's own error.
func TestUnknownOptionIsRejected(t *testing.T) {
	fds := loadDescriptors(t)
	req := &pluginpb.CodeGeneratorRequest{
		FileToGenerate: []string{helloworldProto},
		ProtoFile:      fds.File,
		Parameter:      proto.String("no_such_option=1"),
	}
	var cfg Config
	var flags flag.FlagSet
	cfg.Bind(&flags)
	if _, err := (protogen.Options{ParamFunc: flags.Set}).New(req); err == nil {
		t.Fatal("an unknown option was accepted")
	}
}

// normalizeEOL folds CRLF to LF on the read side.
func normalizeEOL(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

// firstDiff returns a short description of the first differing line, or "".
func firstDiff(want, got string) string {
	if want == got {
		return ""
	}
	w := strings.Split(want, "\n")
	g := strings.Split(got, "\n")
	for i := 0; i < len(w) && i < len(g); i++ {
		if w[i] != g[i] {
			return "line " + itoaTest(i+1) + ":\n  want: " + w[i] + "\n  got:  " + g[i]
		}
	}
	return "want " + itoaTest(len(w)) + " lines, got " + itoaTest(len(g))
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// TestCheckedInFixtureIsFresh compares the fixture the testdata module compiles
// against the current generator output.
//
// Without it the compile proof is worthless: that module would happily build
// whatever stale file is on disk, so the generator could break and CI would
// stay green.
//
// The plugin-version header line is excluded from the comparison. The golden
// pins a fixed version so a release does not rewrite every golden file, while
// the fixture is produced by the real binary and carries the real one; making
// them agree would mean choosing which of those two properties to lose.
func TestCheckedInFixtureIsFresh(t *testing.T) {
	const fixture = "../../testdata/helloworld/poseidon/helloworld_poseidon.pb.go"

	onDisk, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("reading %s: %v\nRegenerate with: cd testdata && buf generate", fixture, err)
	}
	golden, err := os.ReadFile(filepath.Join(goldenDir, "default.golden"))
	if err != nil {
		t.Fatalf("reading the default golden: %v", err)
	}

	if diff := firstDiff(
		dropVersionLine(normalizeEOL(string(golden))),
		dropVersionLine(normalizeEOL(string(onDisk))),
	); diff != "" {
		t.Errorf("%s is stale:\n%s\n\nRegenerate with: cd testdata && buf generate", fixture, diff)
	}
}

// dropVersionLine removes the generator's own version line from a header.
func dropVersionLine(s string) string {
	lines := strings.Split(s, "\n")
	out := lines[:0]
	for _, l := range lines {
		if strings.HasPrefix(l, "// - protoc-gen-go-poseidon ") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// TestRejectsMethodsWithCollidingGoNames covers proto's own uniqueness rule
// not being enough: "SayHello" and "say_hello" are different method names, and
// GoCamelCase maps both to SayHello. The compiler would catch the
// redeclaration — in the USER's build, with no hint of which .proto line caused
// it.
func TestRejectsMethodsWithCollidingGoNames(t *testing.T) {
	const dupProto = "edge/dupmethod.proto"
	p, cfg := newPlugin(t, []string{dupProto}, "")
	err := Run(p, cfg)
	if err == nil {
		t.Fatal("two methods generating one Go name were accepted")
	}
	for _, want := range []string{"say_hello", "SayHello", "edge.Duped"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

// TestRejectsOnlyTheCallersOwnMethodName is the reserved list at its final
// size. Config redeclares the Caller's own method and is refused; Leave sits in
// the same service and is not, because pgrpc.Guard is no longer embedded.
func TestRejectsOnlyTheCallersOwnMethodName(t *testing.T) {
	p, cfg := newPlugin(t, []string{"edge/reserved.proto"}, "")
	err := Run(p, cfg)
	if err == nil {
		t.Fatal("methods named Config and Leave were accepted")
	}
	for _, want := range []string{"Config", "edge.Reserved"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
	// Leave is in that same service and must NOT be refused any more. Asserting
	// its absence is what stops the reserved list from quietly regrowing.
	if strings.Contains(err.Error(), "Leave") {
		t.Errorf("Leave is still reserved; only the Caller's own methods should be: %v", err)
	}
}

// TestUnexportIsUnreachableFromAValidProto records why the unexport failure
// branch has no fixture: protoc refuses non-ASCII identifiers outright
// ("non-ASCII identifiers are not allowed"), and GoCamelCase uppercases the
// first letter of an ASCII name, so the generated service name always starts
// with a rune that HAS a lower case.
//
// The branch stays because it guards an invariant the generator depends on —
// the unexported implementation name must differ from the exported interface
// name — and because nothing in this package would notice if a future protobuf
// release relaxed the identifier rule. This test pins the helper's contract
// directly, since no descriptor can reach it.
func TestUnexportIsUnreachableFromAValidProto(t *testing.T) {
	got, err := unexport("Greeter")
	if err != nil || got != "greeter" {
		t.Fatalf("unexport(Greeter) = %q, %v", got, err)
	}
	if _, err := unexport("世界Svc"); err == nil {
		t.Error("a name whose first rune has no lower case was accepted; the " +
			"implementation struct would then be named the same as the interface")
	}
	if _, err := unexport(""); err == nil {
		t.Error("the empty name was accepted")
	}
}

// TestDirectivesInProtoCommentsAreDefused is the end-to-end half of
// TestDefuseDirectives: a real descriptor, carrying real hostile comments,
// through the real generator.
//
// The assertion is on the OUTPUT, not on the helper, because the helper is only
// safe if every comment path actually calls it. A future emitter that forwards
// a comment some other way would pass the unit test and fail here.
func TestDirectivesInProtoCommentsAreDefused(t *testing.T) {
	p, cfg := newPlugin(t, []string{"comments/comments.proto"}, "")
	_, content := runGen(t, p, cfg)

	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if !strings.HasPrefix(trimmed, "//") || len(trimmed) < 3 {
			continue
		}
		if c := trimmed[2]; c != ' ' && c != '\t' {
			t.Errorf("a compiler directive survived into the output: %q", line)
		}
	}

	// The text itself must still be there; defusing is not censoring.
	for _, want := range []string{"go:build ignore", "line /evil/injected.go:1", "go:generate rm -rf /"} {
		if !strings.Contains(content, want) {
			t.Errorf("the author's text %q was dropped rather than defused", want)
		}
	}
}

// TestCollisionAcrossFilesInOnePackage is the regression for a nameset that
// lived per FILE while the scope it guards is per PACKAGE.
//
// Each file here is internally consistent, so a per-file set sees nothing.
// Together they emit one identifier twice, and the failure used to surface as
// "redeclared in this block" in the USER's build, naming neither .proto.
func TestCollisionAcrossFilesInOnePackage(t *testing.T) {
	p, cfg := newPlugin(t, []string{"multi/a.proto", "multi/b.proto"}, "")
	err := Run(p, cfg)
	if err == nil {
		t.Fatal("two files generating one identifier into one package were accepted")
	}
	for _, want := range []string{"Foo_Bar_Baz_FullMethodName", "multi.Foo_Bar", "multi.Foo"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

// TestOneFileAloneIsFine is the control. Neither file collides with itself, so
// a test that only ever generated one of them would pass on the broken build.
func TestOneFileAloneIsFine(t *testing.T) {
	for _, f := range []string{"multi/a.proto", "multi/b.proto"} {
		t.Run(f, func(t *testing.T) {
			p, cfg := newPlugin(t, []string{f}, "")
			if err := Run(p, cfg); err != nil {
				t.Errorf("%s alone was rejected: %v", f, err)
			}
		})
	}
}

// TestCollisionKeyIsTheOutputDirectory pins the distinction the nameset key
// has to make, and it is a distinction only the OUTPUT PATH can see.
//
// multi/sub/c.proto declares the same go_package as multi/a.proto from a
// different source directory, and generates the same identifier. Under the
// default paths the output is derived from go_package, so both land in one
// directory and the collision is real. Under paths=source_relative each lands
// beside its own .proto, so they are two Go packages and there is nothing to
// collide with — reporting one there is a refusal to generate valid input.
func TestCollisionKeyIsTheOutputDirectory(t *testing.T) {
	files := []string{"multi/a.proto", "multi/sub/c.proto"}

	t.Run("same output directory collides", func(t *testing.T) {
		p, cfg := newPlugin(t, files, "")
		err := Run(p, cfg)
		if err == nil {
			t.Fatal("two files generating one identifier into one directory were accepted")
		}
		if !strings.Contains(err.Error(), "Foo_Bar_Baz_FullMethodName") {
			t.Errorf("the error does not name the identifier: %v", err)
		}
	})

	t.Run("different output directories do not", func(t *testing.T) {
		p, cfg := newPlugin(t, files, "paths=source_relative")
		if err := Run(p, cfg); err != nil {
			t.Errorf("two files in different output directories were reported as colliding: %v", err)
		}
	})
}
