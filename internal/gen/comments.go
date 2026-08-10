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
// lands in the generated file as a directive.
//
// MEASURED, because the first account of this was wrong and I repeated it. A
// generated doc comment is always AFTER the package clause, and that position
// decides what each directive does:
//
//	//go:build ignore   "misplaced compiler directive" — a hard build failure.
//	                    The silent whole-package exclusion people expect needs
//	                    the constraint BEFORE the package clause, which nothing
//	                    a .proto author writes can reach.
//	// +build ignore    inert here. It is a constraint only before the package
//	                    clause, so it is not defused for safety — it is defused
//	                    because it has a space and never matched anyway.
//	//line /evil/x.go:1 HONOURED. This is the one that earns the filter.
//
// The //line case, verified by compiling one:
//
//	/evil/injected.go:4: cannot use "not an int" ... as int value
//
// against a file that does not exist, on a path the .proto author chose. Every
// later diagnostic in the file is reported that way, and a malformed //line
// aborts the generation run outright.
//
// So the build constraint is a broken build we caused, and //line is a lie the
// compiler repeats. Both are worth one space.
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
