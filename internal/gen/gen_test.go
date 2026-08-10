package gen

import "testing"

// TestDefuseDirectives covers the shapes a .proto author can put in a comment
// that stop being prose once they reach Go source.
//
// //line is the one that earns the filter. Verified by compiling one: a
// generated file carrying "//line /evil/injected.go:1" makes the compiler
// report every later error in it against a path that does not exist. A
// //go:build in the same position is a hard "misplaced compiler directive"
// instead — loud, but still a broken build we handed the user.
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
