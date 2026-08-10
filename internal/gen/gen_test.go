package gen

import "testing"

// TestDefuseDirectives covers the shapes a .proto author can put in a comment
// that stop being prose once they reach Go source.
//
// The silent one is the reason this is not cosmetic: with interfaces=false the
// doc is emitted exactly once, so a //go:build constraint is well-formed, the
// build exits 0, and the entire generated package is excluded with no message.
func TestDefuseDirectives(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"build constraint", "//go:build ignore", "// go:build ignore"},
		{"line directive", "//line /evil/injected.go:1", "// line /evil/injected.go:1"},
		{"generate", "//go:generate rm -rf /", "// go:generate rm -rf /"},
		{"a linter pragma is a directive too", "//nolint:gosec", "// nolint:gosec"},
		{"indented, because our layout decides the column", "\t//go:build ignore", "\t// go:build ignore"},
		{"prose is untouched", "// Greeter greets.", "// Greeter greets."},
		{"an empty comment line is untouched", "//", "//"},
		{"only the offending line changes",
			"// Greeter greets.\n//go:build ignore\n// Politely.",
			"// Greeter greets.\n// go:build ignore\n// Politely."},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := defuseDirectives(tc.in); got != tc.want {
				t.Errorf("defuseDirectives(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}
