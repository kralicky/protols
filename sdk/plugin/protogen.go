package plugin

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	sdkutil "github.com/kralicky/protols/sdk/util"

	"github.com/kralicky/protocompile/linker"
	"github.com/kralicky/protols/pkg/lsp"
	"github.com/kralicky/protols/pkg/x/protogen/genid"
	"github.com/kralicky/protols/pkg/x/protogen/strs"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

type plugin struct {
	*protogen.Plugin

	enumsByName    map[protoreflect.FullName]*protogen.Enum
	messagesByName map[protoreflect.FullName]*protogen.Message
}

func New(targets linker.Files, closure []linker.Result, pathMappings lsp.PathMappings) (*protogen.Plugin, error) {
	implicitGoPackagesByDir := make(map[string]string)
	var errs []string

	resultProtos := make([]*descriptorpb.FileDescriptorProto, len(closure))
RESULTS:
	for i, desc := range closure {
		fdp := proto.Clone(desc.FileDescriptorProto()).(*descriptorpb.FileDescriptorProto)

		sanitizePkg := func(goPackage string) string {
			// fix up any incomplete go_package options if we have the info available
			// this will transform e.g. `go_package = "bar"` to `go_package = "github.com/foo/bar"`
			if !strings.Contains(goPackage, ".") && !strings.Contains(goPackage, "/") {
				p := path.Dir(desc.Path())
				if strings.HasSuffix(p, goPackage) {
					return p
				} else {
					return fmt.Sprintf("%s;%s", p, goPackage)
				}
			}
			return goPackage
		}
		if fdp.Options == nil {
			fdp.Options = &descriptorpb.FileOptions{}
		}
		if fdp.Options.GoPackage != nil {
			goPackage := fdp.Options.GetGoPackage()
			if sanitized := sanitizePkg(goPackage); sanitized != goPackage {
				*fdp.Options.GoPackage = sanitized
			}
		} else {
			// if there is no go_package option:
			// - if any other files in the same directory have a go_package option,
			//   and are in the same package, use their go_package.
			// - if sibling files have different packages, we can't infer a go_package
			//   and must skip the file.
			// - iff all files in the directory have no go_package option and all
			//   have the same package, use the directory name as the package name.
			pathDir := path.Dir(desc.Path())
			var canInfer bool
			if _, ok := implicitGoPackagesByDir[pathDir]; !ok {
				canInfer = true
				for _, otherDesc := range closure {
					if otherDesc == desc {
						continue
					}
					if path.Dir(otherDesc.Path()) != pathDir {
						continue
					}

					otherFileOptions := otherDesc.Options().(*descriptorpb.FileOptions)
					if otherFileOptions != nil && otherFileOptions.GoPackage != nil {
						canInfer = false
						if desc.Package() == otherDesc.Package() {
							implicitGoPackagesByDir[pathDir] = sanitizePkg(otherFileOptions.GetGoPackage())
							break
						} else {
							a, b := desc.Package(), otherDesc.Package()
							if b < a {
								a, b = b, a
							}
							errs = append(errs, fmt.Sprintf("could not infer go_package for %s: inconsistent package names in directory %s (%s, %s)", desc.Path(), pathDir, a, b))
							continue RESULTS
						}
					} else if desc.Package() != otherDesc.Package() {
						a, b := desc.Package(), otherDesc.Package()
						if b < a {
							a, b = b, a
						}
						errs = append(errs, fmt.Sprintf("inconsistent package names in directory %s (%s, %s)", pathDir, a, b))
						continue RESULTS
					}
				}
			}
			if _, ok := implicitGoPackagesByDir[pathDir]; !ok {
				// no go_package options in any files in this directory
				if canInfer {
					implicitGoPackagesByDir[pathDir] = pathDir
				}
			}
			if ipath, ok := implicitGoPackagesByDir[pathDir]; ok {
				fdp.Options.GoPackage = &ipath
			}
		}

		resultProtos[i] = fdp
	}
	if errs != nil {
		slices.Sort(errs)
		errs = slices.Compact(errs)
		return nil, errors.New(strings.Join(errs, "; \n"))
	}
	sdkutil.RestoreDescriptorNames(resultProtos, pathMappings)

	gen := &plugin{
		Plugin: &protogen.Plugin{
			Request: &pluginpb.CodeGeneratorRequest{
				ProtoFile: resultProtos,
			},
			FilesByPath: make(map[string]*protogen.File),
			Files:       []*protogen.File{},
		},
		enumsByName:    make(map[protoreflect.FullName]*protogen.Enum),
		messagesByName: make(map[protoreflect.FullName]*protogen.Message),
	}

	for i, lf := range closure {
		gf, err := newFile(gen, lf, resultProtos[i])
		if err != nil {
			return nil, err
		}
		gen.Files = append(gen.Files, gf)
		gen.FilesByPath[lf.Path()] = gf
	}
	for _, t := range targets {
		gen.Request.FileToGenerate = append(gen.Request.FileToGenerate, t.Path())
		gen.FilesByPath[t.Path()].Generate = true
	}
	return gen.Plugin, nil
}

type file struct {
	*protogen.File
	result   linker.Result
	location Location
}

type Location protogen.Location

func (loc Location) appendPath(num protoreflect.FieldNumber, idx int) Location {
	loc.Path = append(protoreflect.SourcePath(nil), loc.Path...) // make copy
	loc.Path = append(loc.Path, int32(num), int32(idx))
	return loc
}

func newFile(gen *plugin, desc linker.Result, proto *descriptorpb.FileDescriptorProto) (*protogen.File, error) {
	goPackagePath := *proto.Options.GoPackage
	var goPackageName string
	if strings.Contains(goPackagePath, ";") {
		goPackagePath, goPackageName, _ = strings.Cut(goPackagePath, ";")
	} else {
		goPackageName = path.Base(goPackagePath)
	}
	f := &file{
		File: &protogen.File{
			Desc:          desc,
			Proto:         proto,
			GoPackageName: protogen.GoPackageName(strs.GoSanitized(goPackageName)),
			GoImportPath:  protogen.GoImportPath(goPackagePath),
		},
		result:   desc,
		location: Location{SourceFile: desc.Path()},
	}

	prefix := proto.GetName()
	if ext := path.Ext(prefix); ext == ".proto" || ext == ".protodevel" {
		prefix = prefix[:len(prefix)-len(ext)]
	}
	prefix = path.Join(goPackagePath, path.Base(prefix))

	f.GoDescriptorIdent = protogen.GoIdent{
		GoName:       "File_" + strs.GoSanitized(proto.GetName()),
		GoImportPath: f.GoImportPath,
	}
	f.GeneratedFilenamePrefix = prefix

	for i, eds := 0, desc.Enums(); i < eds.Len(); i++ {
		f.Enums = append(f.Enums, newEnum(gen, f, nil, eds.Get(i)))
	}
	for i, mds := 0, desc.Messages(); i < mds.Len(); i++ {
		f.Messages = append(f.Messages, newMessage(gen, f, nil, mds.Get(i)))
	}
	for i, xds := 0, desc.Extensions(); i < xds.Len(); i++ {
		f.Extensions = append(f.Extensions, newField(gen, f, nil, xds.Get(i)))
	}
	for i, sds := 0, desc.Services(); i < sds.Len(); i++ {
		f.Services = append(f.Services, newService(gen, f, sds.Get(i)))
	}
	for _, message := range f.Messages {
		if err := resolveMessageDependencies(gen, message); err != nil {
			return nil, err
		}
	}
	for _, extension := range f.Extensions {
		if err := resolveFieldDependencies(gen, extension); err != nil {
			return nil, err
		}
	}
	for _, service := range f.Services {
		for _, method := range service.Methods {
			if err := resolveMethodDependencies(gen, method); err != nil {
				return nil, err
			}
		}
	}

	return f.File, nil
}

func resolveMessageDependencies(gen *plugin, message *protogen.Message) error {
	for _, field := range message.Fields {
		if err := resolveFieldDependencies(gen, field); err != nil {
			return err
		}
	}
	for _, message := range message.Messages {
		if err := resolveMessageDependencies(gen, message); err != nil {
			return err
		}
	}
	for _, extension := range message.Extensions {
		if err := resolveFieldDependencies(gen, extension); err != nil {
			return err
		}
	}
	return nil
}

func resolveFieldDependencies(gen *plugin, field *protogen.Field) error {
	desc := field.Desc
	switch desc.Kind() {
	case protoreflect.EnumKind:
		name := field.Desc.Enum().FullName()
		enum, ok := gen.enumsByName[name]
		if !ok {
			return fmt.Errorf("field %v: no descriptor for enum %v", desc.FullName(), name)
		}
		field.Enum = enum
	case protoreflect.MessageKind, protoreflect.GroupKind:
		name := desc.Message().FullName()
		message, ok := gen.messagesByName[name]
		if !ok {
			return fmt.Errorf("field %v: no descriptor for type %v", desc.FullName(), name)
		}
		field.Message = message
	}
	if desc.IsExtension() {
		name := desc.ContainingMessage().FullName()
		message, ok := gen.messagesByName[name]
		if !ok {
			return fmt.Errorf("field %v: no descriptor for type %v", desc.FullName(), name)
		}
		field.Extendee = message
	}
	return nil
}

func resolveMethodDependencies(gen *plugin, method *protogen.Method) error {
	desc := method.Desc

	inName := desc.Input().FullName()
	in, ok := gen.messagesByName[inName]
	if !ok {
		return fmt.Errorf("method %v: no descriptor for type %v", desc.FullName(), inName)
	}
	method.Input = in

	outName := desc.Output().FullName()
	out, ok := gen.messagesByName[outName]
	if !ok {
		return fmt.Errorf("method %v: no descriptor for type %v", desc.FullName(), outName)
	}
	method.Output = out

	return nil
}

func newEnum(gen *plugin, f *file, parent *protogen.Message, desc protoreflect.EnumDescriptor) *protogen.Enum {
	var loc Location
	if parent != nil {
		loc = Location(parent.Location).appendPath(genid.DescriptorProto_EnumType_field_number, desc.Index())
	} else {
		loc = f.location.appendPath(genid.FileDescriptorProto_EnumType_field_number, desc.Index())
	}
	enum := &protogen.Enum{
		Desc:     desc,
		GoIdent:  newGoIdent(f, desc),
		Location: protogen.Location(loc),
		Values:   make([]*protogen.EnumValue, desc.Values().Len()),
		Comments: makeCommentSet(f.Desc.SourceLocations().ByDescriptor(desc)),
	}
	gen.enumsByName[desc.FullName()] = enum
	for i, l := 0, desc.Values().Len(); i < l; i++ {
		ev := desc.Values().Get(i)
		enum.Values[i] = newEnumValue(gen, f, parent, enum, ev)
	}
	return enum
}

func newEnumValue(gen *plugin, f *file, message *protogen.Message, enum *protogen.Enum, desc protoreflect.EnumValueDescriptor) *protogen.EnumValue {
	// A top-level enum value's name is: EnumName_ValueName
	// An enum value contained in a message is: MessageName_ValueName
	//
	// For historical reasons, enum value names are not camel-cased.
	parentIdent := enum.GoIdent
	if message != nil {
		parentIdent = message.GoIdent
	}
	name := parentIdent.GoName + "_" + string(desc.Name())
	loc := Location(enum.Location).appendPath(genid.EnumDescriptorProto_Value_field_number, desc.Index())
	return &protogen.EnumValue{
		Desc:     desc,
		GoIdent:  f.GoImportPath.Ident(name),
		Parent:   enum,
		Location: protogen.Location(loc),
		Comments: makeCommentSet(f.Desc.SourceLocations().ByDescriptor(desc)),
	}
}

func newMessage(gen *plugin, f *file, parent *protogen.Message, desc protoreflect.MessageDescriptor) *protogen.Message {
	var loc Location
	if parent != nil {
		loc = Location(parent.Location).appendPath(genid.DescriptorProto_NestedType_field_number, desc.Index())
	} else {
		loc = f.location.appendPath(genid.FileDescriptorProto_MessageType_field_number, desc.Index())
	}
	message := &protogen.Message{
		Desc:     desc,
		GoIdent:  newGoIdent(f, desc),
		Location: protogen.Location(loc),
		Comments: makeCommentSet(f.Desc.SourceLocations().ByDescriptor(desc)),
	}
	gen.messagesByName[desc.FullName()] = message
	for i, eds := 0, desc.Enums(); i < eds.Len(); i++ {
		message.Enums = append(message.Enums, newEnum(gen, f, message, eds.Get(i)))
	}
	for i, mds := 0, desc.Messages(); i < mds.Len(); i++ {
		message.Messages = append(message.Messages, newMessage(gen, f, message, mds.Get(i)))
	}
	for i, fds := 0, desc.Fields(); i < fds.Len(); i++ {
		message.Fields = append(message.Fields, newField(gen, f, message, fds.Get(i)))
	}
	for i, ods := 0, desc.Oneofs(); i < ods.Len(); i++ {
		message.Oneofs = append(message.Oneofs, newOneof(gen, f, message, ods.Get(i)))
	}
	for i, xds := 0, desc.Extensions(); i < xds.Len(); i++ {
		message.Extensions = append(message.Extensions, newField(gen, f, message, xds.Get(i)))
	}

	// Resolve local references between fields and oneofs.
	for _, field := range message.Fields {
		if od := field.Desc.ContainingOneof(); od != nil {
			oneof := message.Oneofs[od.Index()]
			field.Oneof = oneof
			oneof.Fields = append(oneof.Fields, field)
		}
	}

	// Field name conflict resolution.
	//
	// We assume well-known method names that may be attached to a generated
	// message type, as well as a 'Get*' method for each field. For each
	// field in turn, we add _s to its name until there are no conflicts.
	//
	// Any change to the following set of method names is a potential
	// incompatible API change because it may change generated field names.
	//
	// TODO: If we ever support a 'go_name' option to set the Go name of a
	// field, we should consider dropping this entirely. The conflict
	// resolution algorithm is subtle and surprising (changing the order
	// in which fields appear in the .proto source file can change the
	// names of fields in generated code), and does not adapt well to
	// adding new per-field methods such as setters.
	usedNames := map[string]bool{
		"Reset":               true,
		"String":              true,
		"ProtoMessage":        true,
		"Marshal":             true,
		"Unmarshal":           true,
		"ExtensionRangeArray": true,
		"ExtensionMap":        true,
		"Descriptor":          true,
	}
	makeNameUnique := func(name string, hasGetter bool) string {
		for usedNames[name] || (hasGetter && usedNames["Get"+name]) {
			name += "_"
		}
		usedNames[name] = true
		usedNames["Get"+name] = hasGetter
		return name
	}
	for _, field := range message.Fields {
		field.GoName = makeNameUnique(field.GoName, true)
		field.GoIdent.GoName = message.GoIdent.GoName + "_" + field.GoName
		if field.Oneof != nil && field.Oneof.Fields[0] == field {
			// Make the name for a oneof unique as well. For historical reasons,
			// this assumes that a getter method is not generated for oneofs.
			// This is incorrect, but fixing it breaks existing code.
			field.Oneof.GoName = makeNameUnique(field.Oneof.GoName, false)
			field.Oneof.GoIdent.GoName = message.GoIdent.GoName + "_" + field.Oneof.GoName
		}
	}

	// Oneof field name conflict resolution.
	//
	// This conflict resolution is incomplete as it does not consider collisions
	// with other oneof field types, but fixing it breaks existing code.
	for _, field := range message.Fields {
		if field.Oneof != nil {
		Loop:
			for {
				for _, nestedMessage := range message.Messages {
					if nestedMessage.GoIdent == field.GoIdent {
						field.GoIdent.GoName += "_"
						continue Loop
					}
				}
				for _, nestedEnum := range message.Enums {
					if nestedEnum.GoIdent == field.GoIdent {
						field.GoIdent.GoName += "_"
						continue Loop
					}
				}
				break Loop
			}
		}
	}

	return message
}

func newField(gen *plugin, f *file, message *protogen.Message, desc protoreflect.FieldDescriptor) *protogen.Field {
	var loc Location
	switch {
	case desc.IsExtension() && message == nil:
		loc = f.location.appendPath(genid.FileDescriptorProto_Extension_field_number, desc.Index())
	case desc.IsExtension() && message != nil:
		loc = Location(message.Location).appendPath(genid.DescriptorProto_Extension_field_number, desc.Index())
	default:
		loc = Location(message.Location).appendPath(genid.DescriptorProto_Field_field_number, desc.Index())
	}
	camelCased := strs.GoCamelCase(string(desc.Name()))
	var parentPrefix string
	if message != nil {
		parentPrefix = message.GoIdent.GoName + "_"
	}
	field := &protogen.Field{
		Desc:   desc,
		GoName: camelCased,
		GoIdent: protogen.GoIdent{
			GoImportPath: f.GoImportPath,
			GoName:       parentPrefix + camelCased,
		},
		Parent:   message,
		Location: protogen.Location(loc),
		Comments: makeCommentSet(f.Desc.SourceLocations().ByDescriptor(desc)),
	}
	return field
}

func newOneof(gen *plugin, f *file, message *protogen.Message, desc protoreflect.OneofDescriptor) *protogen.Oneof {
	loc := Location(message.Location).appendPath(genid.DescriptorProto_OneofDecl_field_number, desc.Index())
	camelCased := strs.GoCamelCase(string(desc.Name()))
	parentPrefix := message.GoIdent.GoName + "_"
	return &protogen.Oneof{
		Desc:   desc,
		Parent: message,
		GoName: camelCased,
		GoIdent: protogen.GoIdent{
			GoImportPath: f.GoImportPath,
			GoName:       parentPrefix + camelCased,
		},
		Location: protogen.Location(loc),
		Comments: makeCommentSet(f.Desc.SourceLocations().ByDescriptor(desc)),
	}
}

func newService(gen *plugin, f *file, desc protoreflect.ServiceDescriptor) *protogen.Service {
	loc := Location(f.location).appendPath(genid.FileDescriptorProto_Service_field_number, desc.Index())
	service := &protogen.Service{
		Desc:     desc,
		GoName:   strs.GoCamelCase(string(desc.Name())),
		Location: protogen.Location(loc),
		Comments: makeCommentSet(f.Desc.SourceLocations().ByDescriptor(desc)),
	}
	for i, mds := 0, desc.Methods(); i < mds.Len(); i++ {
		service.Methods = append(service.Methods, newMethod(gen, f, service, mds.Get(i)))
	}
	return service
}

func newMethod(gen *plugin, f *file, service *protogen.Service, desc protoreflect.MethodDescriptor) *protogen.Method {
	loc := Location(service.Location).appendPath(genid.ServiceDescriptorProto_Method_field_number, desc.Index())
	method := &protogen.Method{
		Desc:     desc,
		GoName:   strs.GoCamelCase(string(desc.Name())),
		Parent:   service,
		Location: protogen.Location(loc),
		Comments: makeCommentSet(f.Desc.SourceLocations().ByDescriptor(desc)),
	}
	return method
}

// newGoIdent returns the Go identifier for a descriptor.
func newGoIdent(f *file, d protoreflect.Descriptor) protogen.GoIdent {
	name := strings.TrimPrefix(string(d.FullName()), string(f.Desc.Package())+".")
	return protogen.GoIdent{
		GoName:       strs.GoCamelCase(name),
		GoImportPath: f.GoImportPath,
	}
}

func makeCommentSet(loc protoreflect.SourceLocation) protogen.CommentSet {
	var leadingDetached []protogen.Comments
	for _, s := range loc.LeadingDetachedComments {
		leadingDetached = append(leadingDetached, protogen.Comments(s))
	}
	return protogen.CommentSet{
		LeadingDetached: leadingDetached,
		Leading:         protogen.Comments(loc.LeadingComments),
		Trailing:        protogen.Comments(loc.TrailingComments),
	}
}
