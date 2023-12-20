package python

import (
	"fmt"
	"slices"
	"strings"

	"github.com/iancoleman/strcase"
	"github.com/kralicky/protols/sdk/codegen/generators/x/python/util"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

type Model struct {
	OutputFile *OutputFile `json:"output_file"`
}

type OutputFile struct {
	InputFilenames          []string  `json:"input_filenames"`
	Imports                 []string  `json:"imports"`
	DatetimeImports         []string  `json:"datetime_imports"`
	PythonModuleImports     []string  `json:"python_module_imports"`
	ImportsTypeCheckingOnly []string  `json:"imports_type_checking_only"`
	TypingImports           []string  `json:"typing_imports"`
	Enums                   []Enum    `json:"enums"`
	Messages                []Message `json:"messages"`
	Services                []Service `json:"services"`
}

type Enum struct {
	Comment string  `json:"comment"`
	PyName  string  `json:"py_name"`
	Entries []Entry `json:"entries"`
}

type Entry struct {
	Comment string `json:"comment"`
	Name    string `json:"name"`
	Value   int32  `json:"value"`
}

type Message struct {
	Comment             string  `json:"comment"`
	Deprecated          bool    `json:"deprecated"`
	PyName              string  `json:"py_name"`
	HasDeprecatedFields bool    `json:"has_deprecated_fields"`
	DeprecatedFields    []Field `json:"deprecated_fields"`
	Fields              []Field `json:"fields"`
}

type Field struct {
	FieldString string `json:"field_string"`
	Comment     string `json:"comment"`
}

type Service struct {
	Comment string   `json:"comment"`
	PyName  string   `json:"py_name"`
	Methods []Method `json:"methods"`
}

type Method struct {
	PyInputMessageParam string `json:"py_input_message_param"`
	Comment             string `json:"comment"`
	PyOutputMessageType string `json:"py_output_message_type"`
	PyInputMessageType  string `json:"py_input_message_type"`
	PyName              string `json:"py_name"`
	Route               string `json:"route"`
	PyInputMessage      string `json:"py_input_message"`
	ServerStreaming     bool   `json:"server_streaming"`
	ClientStreaming     bool   `json:"client_streaming"`
}

func buildModel(f protoreflect.FileDescriptor) *Model {
	m := &Model{
		OutputFile: &OutputFile{
			InputFilenames: []string{f.Path()},
		},
	}
	m.OutputFile.Enums = m.buildEnums(f)
	m.OutputFile.Messages = m.buildMessages(f)
	m.OutputFile.Services = m.buildServices(f)
	m.OutputFile.cleanImports()

	return m
}

func (m *Model) buildEnums(f protoreflect.FileDescriptor) []Enum {
	enums := []Enum{}
	srcLocations := f.SourceLocations()
	for _, e := range util.Collect(f.Enums()) {
		entries := []Entry{}
		for _, value := range util.Collect(e.Values()) {
			e := Entry{
				Comment: formatComment(srcLocations.ByDescriptor(value).LeadingComments, 1),
				Name:    string(value.Name()),
				Value:   int32(value.Number()),
			}
			entries = append(entries, e)
		}
		enums = append(enums, Enum{
			Comment: formatComment(srcLocations.ByDescriptor(e).LeadingComments, 1),
			PyName:  formatClassName(e.Name()),
			Entries: entries,
		})
	}
	for _, msg := range util.Collect(f.Messages()) {
		for _, nestedEnum := range util.Collect(msg.Enums()) {
			entries := []Entry{}
			for _, value := range util.Collect(nestedEnum.Values()) {
				e := Entry{
					Comment: formatComment(srcLocations.ByDescriptor(value).LeadingComments, 1),
					Name:    string(value.Name()),
					Value:   int32(value.Number()),
				}
				entries = append(entries, e)
			}
			enums = append(enums, Enum{
				Comment: formatComment(srcLocations.ByDescriptor(nestedEnum).LeadingComments, 1),
				PyName:  formatClassName(msg.Name() + "_" + nestedEnum.Name()),
				Entries: entries,
			})
		}
	}
	return enums
}

func (m *Model) buildMessages(f protoreflect.FileDescriptor) []Message {
	srcLocations := f.SourceLocations()
	messages := []Message{}
	for _, msg := range util.Collect(f.Messages()) {
		fields := []Field{}
		for _, field := range util.Collect(msg.Fields()) {
			fields = append(fields, m.buildField(field))
		}
		messages = append(messages, Message{
			Comment:    formatComment(srcLocations.ByDescriptor(msg).LeadingComments, 1),
			PyName:     formatClassName(msg.Name()),
			Deprecated: msg.Options().(*descriptorpb.MessageOptions).GetDeprecated(),
			Fields:     fields,
		})
	}
	return messages
}

func (m *Model) buildField(f protoreflect.FieldDescriptor) Field {
	name := formatFieldName(f.Name())
	annotation := ""
	pyType, err := m.pyType(f)
	if err != nil {
		panic(err)
	}
	fieldWraps := f.Kind() == protoreflect.MessageKind && strings.HasPrefix(string(f.Message().FullName()), "google.protobuf")
	fieldArgs := []string{fmt.Sprint(f.Number())}
	if fieldWraps {
		fieldArgs = append(fieldArgs, "wraps=True")
	}
	var protoFieldType string

	if f.IsMap() {
		keyType, err := m.pyType(f.MapKey())
		if err != nil {
			panic(err)
		}
		valueType, err := m.pyType(f.MapValue())
		if err != nil {
			panic(err)
		}
		annotation = fmt.Sprintf(": Dict[%s, %s]", keyType, valueType)
		fieldArgs = append(fieldArgs, fmt.Sprintf("key_type=%s", keyType), fmt.Sprintf("value_type=%s", valueType))
		m.OutputFile.TypingImports = append(m.OutputFile.TypingImports, "Dict")
	} else if f.Cardinality() == protoreflect.Repeated {
		annotation = fmt.Sprintf(": List[%s]", pyType)
		m.OutputFile.TypingImports = append(m.OutputFile.TypingImports, "List")
	} else {
		annotation = fmt.Sprintf(": %s", pyType)
	}

	if f.IsMap() {
		protoFieldType = "map"
	} else {
		protoFieldType = pyType
	}
	fieldType := fmt.Sprintf("betterproto.%s_field(%s)", protoFieldType, strings.Join(fieldArgs, ", "))
	return Field{
		Comment:     formatComment(f.ParentFile().SourceLocations().ByDescriptor(f).LeadingComments, 1),
		FieldString: fmt.Sprintf("%s%s = %s", name, annotation, fieldType),
	}
}

func (m *Model) messageTypeRef(from protoreflect.FileDescriptor, msg protoreflect.MessageDescriptor) string {
	fieldWraps := strings.HasPrefix(string(msg.FullName()), "google.protobuf")
	return m.getTypeReference(from, msg.ParentFile(),
		msg.ParentFile().Imports(),
		string(msg.FullName()), fieldWraps)
}

func (m *Model) buildServices(f protoreflect.FileDescriptor) []Service {
	anyClientStreaming := false
	anyServerStreaming := false
	srcLocations := f.SourceLocations()
	services := []Service{}
	for _, s := range util.Collect(f.Services()) {
		methods := []Method{}
		for _, method := range util.Collect(s.Methods()) {

			methods = append(methods, Method{
				PyInputMessageParam: formatFieldName(method.Input().Name()),
				Comment:             formatComment(srcLocations.ByDescriptor(method).LeadingComments, 2),
				PyOutputMessageType: m.messageTypeRef(f, method.Output()),
				PyInputMessageType:  m.messageTypeRef(f, method.Input()),
				PyName:              formatMethodName(method.Name()),
				Route:               fmt.Sprintf("/%s/%s", s.FullName(), method.Name()),
				PyInputMessage:      formatClassName(method.Input().Name()),
				ServerStreaming:     method.IsStreamingServer(),
				ClientStreaming:     method.IsStreamingClient(),
			})
			if method.IsStreamingClient() {
				anyClientStreaming = true
			}
			if method.IsStreamingServer() {
				anyServerStreaming = true
			}
		}
		services = append(services, Service{
			Comment: formatComment(srcLocations.ByDescriptor(s).LeadingComments, 1),
			PyName:  formatClassName(s.Name()),
			Methods: methods,
		})
	}
	if len(services) > 0 {
		m.OutputFile.TypingImports = append(m.OutputFile.TypingImports, "Dict", "Optional")
		m.OutputFile.ImportsTypeCheckingOnly = append(m.OutputFile.ImportsTypeCheckingOnly,
			"from betterproto.grpc.grpclib_client import MetadataLike",
			"from grpclib.metadata import Deadline",
		)
	}
	if anyClientStreaming {
		m.OutputFile.TypingImports = append(m.OutputFile.TypingImports, "AsyncIterable", "Iterable", "Union")
	}
	if anyClientStreaming || anyServerStreaming {
		m.OutputFile.TypingImports = append(m.OutputFile.TypingImports, "AsyncIterator")
	}
	return services
}

func formatClassName[T ~string](name T) string {
	return strcase.ToCamel(string(name))
}

func formatFieldName[T ~string](name T) string {
	return strcase.ToSnake(string(name))
}

func formatMethodName[T ~string](name T) string {
	return strcase.ToSnake(string(name))
}

func (m *Model) pyType(field protoreflect.FieldDescriptor) (string, error) {
	switch field.Kind() {
	case protoreflect.DoubleKind,
		protoreflect.FloatKind:
		return "float", nil
	case protoreflect.Int64Kind,
		protoreflect.Uint64Kind,
		protoreflect.Int32Kind,
		protoreflect.Uint32Kind,
		protoreflect.Fixed64Kind,
		protoreflect.Fixed32Kind,
		protoreflect.Sfixed64Kind,
		protoreflect.Sfixed32Kind,
		protoreflect.Sint32Kind,
		protoreflect.Sint64Kind:
		return "int", nil
	case protoreflect.BoolKind:
		return "bool", nil
	case protoreflect.StringKind:
		return "str", nil
	case protoreflect.BytesKind:
		return "bytes", nil
	case protoreflect.MessageKind:
		if field.IsMap() {
			keyType, err := m.pyType(field.MapKey())
			if err != nil {
				return "", err
			}
			valueType, err := m.pyType(field.MapValue())
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("Dict[%s, %s]", keyType, valueType), nil
		}
		fieldWraps := strings.HasPrefix(string(field.Message().FullName()), "google.protobuf")
		return m.getTypeReference(
			field.ParentFile(), field.Message().ParentFile(),
			field.ParentFile().Imports(),
			string(field.Message().FullName()), fieldWraps), nil
	case protoreflect.EnumKind:
		return m.getTypeReference(
			field.ParentFile(), field.Enum().ParentFile(),
			field.ParentFile().Imports(),
			string(field.Enum().FullName()), false), nil
	}
	return "", fmt.Errorf("unknown/unsupported type %s", field.Kind())
}

func (o *OutputFile) cleanImports() {
	slices.Sort(o.Imports)
	slices.Sort(o.DatetimeImports)
	slices.Sort(o.ImportsTypeCheckingOnly)
	slices.Sort(o.PythonModuleImports)
	slices.Sort(o.TypingImports)
	o.Imports = slices.Compact(o.Imports)
	o.DatetimeImports = slices.Compact(o.DatetimeImports)
	o.ImportsTypeCheckingOnly = slices.Compact(o.ImportsTypeCheckingOnly)
	o.PythonModuleImports = slices.Compact(o.PythonModuleImports)
	o.TypingImports = slices.Compact(o.TypingImports)
}

func formatComment(comment string, indent int) string {
	var lines []string
	for _, line := range strings.Split(comment, "\n") {
		line = strings.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return ""
	}
	space := strings.Repeat(" ", indent*4)
	if len(lines) == 1 {
		return fmt.Sprintf(space+`""" %s """`, lines[0])
	}

	buf := strings.Builder{}
	buf.WriteString(space + `"""` + "\n")
	for _, line := range lines {
		buf.WriteString(space + line + "\n")
	}
	buf.WriteString(space + `"""`)
	return buf.String()
}
