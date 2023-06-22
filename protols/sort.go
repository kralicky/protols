package main

import "github.com/jhump/protoreflect/desc/protoprint"

// Sort logic w.r.t. the file:
// 1. Package is always first, and imports are always second
// 2. Builtin options are always third. Custom options are not sorted.
// 3. Messages, enums, services, and extensions are not sorted.

// Sort logic w.r.t. individual elements:
// 1. If the elements have a number, sort by number w.r.t. other numbered elements.
// 2. Otherwise, they are not sorted (i.e. they are left in the order they appear in the file).

func SortElements(a, b protoprint.Element) (less bool) {
	// First, sort by kind of element. The "less" function will return true if a should come before b.
	if a.Kind() != b.Kind() && a.Kind() <= protoprint.KindOption && b.Kind() <= protoprint.KindOption {
		return a.Kind() < b.Kind()
	}
	// At this point, a and b are of the same kind. We apply different sorting rules based on the kind.
	switch a.Kind() {
	case protoprint.KindOption:
		// Builtin options come before custom options.
		if a.IsCustomOption() != b.IsCustomOption() {
			return !a.IsCustomOption() && b.IsCustomOption()
		}
		// If both are builtin or both are custom, do not sort.
		return false
	case protoprint.KindField, protoprint.KindExtension, protoprint.KindEnumValue:
		// Sort by number.
		return a.Number() < b.Number()
	case protoprint.KindImport:
		// Sort by path.
		return a.Name() < b.Name()
	default:
		// For all other kinds, do not sort.
		return false
	}
}
