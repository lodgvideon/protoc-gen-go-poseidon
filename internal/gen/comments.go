package gen

import "strings"

// defuseDirectives makes a .proto comment safe to paste into Go source.
//
// A Go comment whose "//" is followed IMMEDIATELY by a non-space is not prose,
// it is a compiler directive. protoc strips the "//" from a .proto comment and
// protogen puts one back, so a .proto that says
//
//	//go:build ignore
//	service Greeter { ... }
//
// lands in the generated file as a real build constraint. Under the default
// options the doc is emitted more than once and the build fails with "multiple
// //go:build comments"; with interfaces=false it is emitted exactly once, and
// then the failure goes SILENT — go build exits 0 and the whole generated
// package is excluded with no message at all.
//
// A //line directive is worse than either. It is honoured, so every later
// compiler diagnostic in the file is reported against a path the .proto author
// chose, and a malformed one aborts the generation run outright.
//
// The fix is one space. The author's text survives and is prose again.
//
// EVERY directive shape is defused rather than a list of known names. The form
// is general — "//tool:directive" — tools add new ones, and a filter that
// enumerates today's names silently stops covering tomorrow's. Indentation is
// ignored for the same kind of reason: a directive is only honoured at column
// zero, but this generator's own layout decides the column, so a comment that
// is harmless inside an interface today becomes a directive the day it is
// emitted as a top-level doc.
func defuseDirectives(s string) string {
	if s == "" {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		trimmed := strings.TrimLeft(l, " \t")
		if !strings.HasPrefix(trimmed, "//") || len(trimmed) < 3 {
			continue
		}
		if c := trimmed[2]; c == ' ' || c == '\t' {
			continue
		}
		lines[i] = l[:len(l)-len(trimmed)] + "// " + trimmed[2:]
	}
	return strings.Join(lines, "\n")
}
