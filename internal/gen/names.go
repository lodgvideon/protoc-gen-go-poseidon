package gen

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"google.golang.org/protobuf/compiler/protogen"
)

// nameset records every package-scope identifier this generator emits into one
// output package, so that a collision is a clear error rather than a compile
// failure in the user's build.
//
// The sub-package default already makes collisions with protoc-gen-go and
// protoc-gen-go-grpc impossible — distinct package blocks are disjoint scopes —
// so this guards only what remains: our OWN names colliding with each other.
// That is not hypothetical. `service Foo_Bar` with method `Baz` and `service
// Foo` with method `Bar_Baz` both yield Foo_Bar_Baz_FullMethodName, and several
// .proto files can route into one output package.
type nameset struct {
	owners map[string]string
	// conflicts accumulates rather than returning early, because protogen's
	// Error keeps only the FIRST error — reporting one at a time would make the
	// user re-run per fix.
	conflicts []string
	pkg       string
}

func newNameset(pkg string) *nameset {
	return &nameset{owners: make(map[string]string, 32), pkg: pkg}
}

// claim records name as ours and reports the conflict if it is already taken.
// It returns name, so it can wrap an expression at its use site.
func (n *nameset) claim(name, owner string) string {
	if prev, taken := n.owners[name]; taken {
		n.conflicts = append(n.conflicts,
			fmt.Sprintf("  %s\n    claimed by: %s\n    also by:    %s", name, prev, owner))
		return name
	}
	n.owners[name] = owner
	return name
}

// err returns the accumulated conflicts as one error, or nil.
func (n *nameset) err() error {
	if len(n.conflicts) == 0 {
		return nil
	}
	sort.Strings(n.conflicts)
	// No trailing full stop or newline: staticcheck's ST1005 forbids both, and
	// protoc prefixes this with the plugin's name when it surfaces the failure.
	return fmt.Errorf("identifier collision in generated package %q:\n%s\n"+
		"fix: rename the conflicting .proto symbol, or set package_suffix to route them apart",
		n.pkg, strings.Join(n.conflicts, "\n"))
}

// reservedMethodNames are method names a service must not have, because the
// generated Caller would break in a way the compiler does not always catch.
//
// Config is a plain redeclaration and fails to build. Enter and Leave are the
// dangerous ones: *XCaller embeds pgrpc.Guard, so a generated method with
// either name SHADOWS the promoted one by Go's depth rule — and the generated
// body's own `x.Enter()` then calls itself. Infinite recursion, no diagnostic.
var reservedMethodNames = map[string]string{
	"Config": "the Caller's own Config method",
	"Enter":  "pgrpc.Guard.Enter, promoted onto the Caller",
	"Leave":  "pgrpc.Guard.Leave, promoted onto the Caller",
}

// checkService rejects the shapes that would produce broken generated code
// regardless of what else is in the package.
func checkService(s *protogen.Service, callers bool) error {
	var problems []string

	seen := make(map[string]string, len(s.Methods))
	for _, m := range s.Methods {
		// Two .proto method names can camel-case to one Go name — "SayHello"
		// and "say_hello" both become SayHello — and proto's own uniqueness
		// rule does not prevent it.
		if prev, dup := seen[m.GoName]; dup {
			problems = append(problems, fmt.Sprintf(
				"methods %q and %q both generate the Go name %s", prev, m.Desc.Name(), m.GoName))
			continue
		}
		seen[m.GoName] = string(m.Desc.Name())

		if callers {
			if why, bad := reservedMethodNames[m.GoName]; bad {
				problems = append(problems, fmt.Sprintf(
					"method %q generates the Go name %s, which collides with %s",
					m.Desc.Name(), m.GoName, why))
			}
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("service %s cannot be generated:\n  %s",
		s.Desc.FullName(), strings.Join(problems, "\n  "))
}

// unexport lowercases the first rune of an exported identifier.
//
// It is rune-aware rather than protoc-gen-go-grpc's strings.ToLower(s[:1]) +
// s[1:], which slices a multi-byte first rune in half and produces invalid
// UTF-8 for a service like "Ünary". It also reports failure rather than
// returning the input unchanged: for a service whose first rune has no lower
// case — "世界Svc" — the struct name would otherwise equal the interface name,
// which is a redeclaration.
func unexport(s string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("empty identifier")
	}
	r, size := utf8.DecodeRuneInString(s)
	lower := unicode.ToLower(r)
	if lower == r {
		return "", fmt.Errorf("the first rune of %q has no lower case, so the unexported "+
			"implementation name would collide with the exported interface name", s)
	}
	return string(lower) + s[size:], nil
}
