package format

import "github.com/jhump/protoreflect/desc/protoprint"

// Sort logic w.r.t. the file:
// 1. Package is always first, and imports are always second
// 2. Builtin options are always third. Custom options are not sorted.
// 3. Messages, enums, services, and extensions are not sorted.

// Sort logic w.r.t. individual elements:
// 1. If the elements have a number, sort by number w.r.t. other numbered elements.
// 2. Otherwise, they are not sorted (i.e. they are left in the order they appear in the file).

func SortElements(a, b protoprint.Element, ignore func() bool) (less bool) {
	if a.Kind() == b.Kind() {
		switch a.Kind() {
		case protoprint.KindOption:
			if a.IsCustomOption() != b.IsCustomOption() {
				return !a.IsCustomOption()
			}
		case protoprint.KindField, protoprint.KindExtension, protoprint.KindEnumValue:
			return a.Number() < b.Number()
		case protoprint.KindImport:
			return a.Name() < b.Name()
		}
		return ignore()
	}

	if a.Kind() == protoprint.KindPackage {
		return true
	} else if b.Kind() == protoprint.KindPackage {
		return false
	}

	if a.Kind() == protoprint.KindImport {
		return true
	} else if b.Kind() == protoprint.KindImport {
		return false
	}

	if a.Kind() == protoprint.KindOption {
		return true
	} else if b.Kind() == protoprint.KindOption {
		return false
	}

	return ignore()
}
