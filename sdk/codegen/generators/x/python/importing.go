package python

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/iancoleman/strcase"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// https://github.com/danielgtaylor/python-betterproto/blob/master/src/betterproto/compile/importing.py

var wrapperTypes = map[string]string{
	"google.protobuf.DoubleValue": "google_protobuf.DoubleValue",
	"google.protobuf.FloatValue":  "google_protobuf.FloatValue",
	"google.protobuf.Int32Value":  "google_protobuf.Int32Value",
	"google.protobuf.Int64Value":  "google_protobuf.Int64Value",
	"google.protobuf.UInt32Value": "google_protobuf.UInt32Value",
	"google.protobuf.UInt64Value": "google_protobuf.UInt64Value",
	"google.protobuf.BoolValue":   "google_protobuf.BoolValue",
	"google.protobuf.StringValue": "google_protobuf.StringValue",
	"google.protobuf.BytesValue":  "google_protobuf.BytesValue",
}

func parseSourceTypeName(fieldTypeName string) (string, string) {
	packageMatch := regexp.MustCompile(`^\.?([^A-Z]+)\.(.+)`)
	if packageMatch.Match([]byte(fieldTypeName)) {
		packageName := packageMatch.FindStringSubmatch(fieldTypeName)[1]
		typeName := packageMatch.FindStringSubmatch(fieldTypeName)[2]
		return packageName, typeName
	} else {
		return "", strings.TrimPrefix(fieldTypeName, ".")
	}
}

func (m *Model) getTypeReference(from, to protoreflect.FileDescriptor, imports protoreflect.FileImports, sourceType string, unwrap bool) string {
	if unwrap {
		if wrapperType, ok := wrapperTypes[sourceType]; ok {
			m.OutputFile.TypingImports = append(m.OutputFile.TypingImports, "Optional")
			return fmt.Sprintf("Optional[%s]", wrapperType)
		}
		if sourceType == "google.protobuf.Duration" {
			m.OutputFile.DatetimeImports = append(m.OutputFile.DatetimeImports, "timedelta")
			return "timedelta"
		}
		if sourceType == "google.protobuf.Timestamp" {
			m.OutputFile.DatetimeImports = append(m.OutputFile.DatetimeImports, "datetime")
			return "datetime"
		}
	}
	sourcePackage, sourceType := parseSourceTypeName(sourceType)
	pkg := string(from.ParentFile().Package())
	currentPackage := strings.Split(pkg, ".")
	pyPackage := strings.Split(sourcePackage, ".")
	pyType := formatClassName(sourceType)

	if slices.Equal(pyPackage, []string{"google", "protobuf"}) {
		pyPackage = append([]string{"betterproto", "lib"}, pyPackage...)
	}

	var ref string
	var addImports []string
	switch {
	case len(pyPackage) > 0 && slices.Equal(pyPackage[:1], []string{"betterproto"}):
		ref, addImports = referenceAbsolute(imports, pyPackage, pyType)
	case slices.Equal(pyPackage, currentPackage):
		ref, addImports = referenceSibling(from, to, pyType)
	case len(pyPackage) >= len(currentPackage) && slices.Equal(pyPackage[:len(currentPackage)], currentPackage):
		ref, addImports = referenceDescendent(currentPackage, imports, pyPackage, pyType)
	case len(currentPackage) >= len(pyPackage) && slices.Equal(currentPackage[:len(pyPackage)], pyPackage):
		ref, addImports = referenceAncestor(currentPackage, imports, pyPackage, pyType)
	default:
		ref, addImports = referenceCousin(currentPackage, imports, pyPackage, pyType)
	}
	m.OutputFile.Imports = append(m.OutputFile.Imports, addImports...)
	return ref
}

func referenceAbsolute(imports protoreflect.FileImports, pyPackage []string, pyType string) (ref string, addImports []string) {
	stringImport := strings.Join(pyPackage, ".")
	stringAlias := strcase.ToSnake(stringImport)
	return fmt.Sprintf("%s.%s", stringAlias, pyType), []string{fmt.Sprintf("import %s as %s", stringImport, stringAlias)}
}

func referenceSibling(from, to protoreflect.FileDescriptor, pyType string) (_ string, addImports []string) {
	// check if the files are the same, if not we need to add an import
	if from.Path() != to.ParentFile().Path() {
		filename := strings.TrimSuffix(filepath.Base(string(to.Name())), filepath.Ext(string(to.Name()))) + "_pb"
		addImports = append(addImports, fmt.Sprintf("from %s import %s", filename, pyType))
	}
	return pyType, addImports
}

func referenceDescendent(currentPackage []string, imports protoreflect.FileImports, pyPackage []string, pyType string) (ref string, addImports []string) {
	importingDescendent := pyPackage[len(currentPackage):]
	stringFrom := strings.Join(importingDescendent[:len(importingDescendent)-1], ".")
	stringImport := importingDescendent[len(importingDescendent)-1]
	if stringFrom != "" {
		stringAlias := strings.Join(importingDescendent, "_")
		addImports = append(addImports, fmt.Sprintf("from .%s import %s as %s", stringFrom, stringImport, stringAlias))
		ref = fmt.Sprintf("%s.%s", stringAlias, pyType)
		return
	}
	addImports = append(addImports, fmt.Sprintf("from . import %s", stringImport))
	ref = fmt.Sprintf("%s.%s", stringImport, pyType)
	return
}

func referenceAncestor(currentPackage []string, imports protoreflect.FileImports, pyPackage []string, pyType string) (ref string, addImports []string) {
	distanceUp := len(currentPackage) - len(pyPackage)
	if len(pyPackage) > 0 {
		stringImport := pyPackage[len(pyPackage)-1]
		stringAlias := fmt.Sprintf("_%s%s__", strings.Repeat("_", distanceUp), stringImport)
		stringFrom := fmt.Sprintf("..%s", strings.Repeat(".", distanceUp))
		addImports = append(addImports, fmt.Sprintf("from %s import %s as %s", stringFrom, stringImport, stringAlias))
		ref = fmt.Sprintf("%s.%s", stringAlias, pyType)
		return
	}
	stringAlias := fmt.Sprintf("_%s%s__", strings.Repeat("_", distanceUp), pyType)
	addImports = append(addImports, fmt.Sprintf("from .%s import %s as %s", strings.Repeat(".", distanceUp), pyType, stringAlias))
	ref = stringAlias
	return
}

func referenceCousin(currentPackage []string, imports protoreflect.FileImports, pyPackage []string, pyType string) (ref string, addImports []string) {
	index := 0
	for ; index < len(currentPackage) && index < len(pyPackage) && currentPackage[index] == pyPackage[index]; index++ {
	}

	sharedAncestry := currentPackage[:index]
	distanceUp := len(currentPackage) - len(sharedAncestry)
	stringFrom := fmt.Sprintf(".%s", strings.Repeat(".", distanceUp)) + strings.Join(pyPackage[len(sharedAncestry):len(pyPackage)-1], ".")
	stringImport := pyPackage[len(pyPackage)-1]
	// Add trailing __ to avoid name mangling (python.org/dev/peps/pep-0008/#id34)
	stringAlias := strings.Repeat("_", distanceUp) +
		strcase.ToSnake(strings.Join(pyPackage[len(sharedAncestry):], ".")) +
		"__"
	addImports = append(addImports, fmt.Sprintf("from %s import %s as %s", stringFrom, stringImport, stringAlias))
	ref = fmt.Sprintf("%s.%s", stringAlias, pyType)
	return
}
