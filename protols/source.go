package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/bufbuild/protocompile/ast"
	protocol "go.lsp.dev/protocol"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

var sentinel = errors.New("sentinel")

func findNodeAtSourcePos(file *ast.FileNode, pos ast.SourcePos) []ast.Node {
	var path []ast.Node
	var tracker ast.AncestorTracker
	err := ast.Walk(file, &ast.SimpleVisitor{DoVisitNode: func(n ast.Node) error {
		info := file.NodeInfo(n)
		if locationIsWithinNode(pos, info) {
			if _, ok := n.(ast.TerminalNode); ok {
				path = tracker.Path()
				return sentinel
			}
		}
		return nil
	}}, tracker.AsWalkOptions()...)
	if err != nil && err != sentinel {
		return nil
	}
	return path
}

func locationIsWithinNode(location ast.SourcePos, nodeInfo ast.NodeInfo) bool {
	var (
		start = nodeInfo.Start()
		end   = nodeInfo.End()
	)
	// This is an "open range", so the locaton.Column() must be strictly
	// less than the end.Col.
	return location.Line >= start.Line && location.Line <= end.Line && location.Col >= start.Col && location.Col < end.Col
}

func findRelevantDescriptorAtLocation(params *protocol.TextDocumentPositionParams, snap *Snapshot, lg *zap.Logger) (protoreflect.Descriptor, *protocol.Range, error) {
	fd, err := snap.FindFileByPath(params.TextDocument.URI.Filename())
	if err != nil {
		return nil, nil, err
	}
	sourcePos := ast.SourcePos{
		Filename: params.TextDocument.URI.Filename(),
		Line:     int(params.Position.Line) + 1,
		Col:      int(params.Position.Character) + 1,
	}

	fileNode := fd.AST()
	// find the node in the ast at the given position
	path := findNodeAtSourcePos(fileNode, sourcePos)
	if len(path) == 0 {
		return nil, nil, errors.New("no node found at position")
	}
	// squash compound identifiers at the end of the path
	// if len(path) > 1 {
	// 	if ident, ok := path[len(path)-1].(*ast.IdentNode); ok {
	// 		if compoundIdent, ok := path[len(path)-2].(*ast.CompoundIdentNode); ok {
	// 			if compoundIdent.Components[len(compoundIdent.Components)-1].Val == ident.Val {
	// 				lg.Debug("squashing compound identifier")
	// 				path = path[:len(path)-1]
	// 			}
	// 		}
	// 	}
	// }

	closestIdentifiableIndex := -1
	var descriptor proto.Message
	for i := len(path) - 1; i >= 0; i-- {
		item := path[i]
		desc := fd.Descriptor(item)
		if desc == nil {
			lg.With(
				zap.Any("node", item),
				zap.String("type", fmt.Sprintf("%T", item)),
			).Debug("no descriptor for this node")
			continue
		}
		lg.With(
			zap.String("type", fmt.Sprintf("%T", item)),
		).Debug("found descriptor for this node")
		closestIdentifiableIndex = i
		descriptor = desc
		break
	}

	if closestIdentifiableIndex == -1 {
		return nil, nil, errors.New("no identifiable node found at position")
	}
	var definitionFullName protoreflect.FullName
	if closestIdentifiableIndex == len(path)-1 {
		lg.With(
			zap.Any("descriptor", descriptor),
			zap.String("type", fmt.Sprintf("%T", descriptor)),
		).Debug("descriptor for this identifier is directly mapped")
	} else {
		lg.With(
			zap.Int("closestIndex", closestIdentifiableIndex),
			zap.Int("pathLength", len(path)),
			zap.Any("closest", descriptor.ProtoReflect().Descriptor().Name()),
			zap.String("closest", fmt.Sprintf("%T", descriptor)),
		).Debug("identifier is not directly mapped")

		// special cases:

		switch descriptor := descriptor.(type) {
		// 1. Imports, which resolve to ambiguous file descriptors
		case *descriptorpb.FileDescriptorProto:
			if lit, ok := path[len(path)-1].(*ast.StringLiteralNode); ok {
				if importNode, ok := path[len(path)-2].(*ast.ImportNode); ok {
					lg.Debug("special case: hovering over import")
					filename := importNode.Name.AsString()
					f, err := snap.Files.AsResolver().FindFileByPath(filename)
					if err != nil {
						f, err = protoregistry.GlobalFiles.FindFileByPath(filename)
						if err != nil {
							return nil, nil, fmt.Errorf("could not find file %q: %w", filename, err)
						}
					}
					rng := toRange(fileNode.NodeInfo(lit))
					return f, &rng, nil
				}
			}

		// 2. Synthetic map fields
		case *descriptorpb.DescriptorProto:
			switch field := path[closestIdentifiableIndex].(type) {
			case *ast.MapFieldNode:
				switch ident := path[len(path)-1].(type) {
				case *ast.MapTypeNode:
					// hovering over one of the map types
					lg.Debug("special case: hovering over map type")
				case *ast.IdentNode:
					switch ident.Val {
					case field.Name.Val:
						// hovering over the map field name, so return the field
						lg.Debug("special case: hovering over map field name")
						// go up one more level
						mapNode := path[closestIdentifiableIndex-1]
						if msgNode, ok := mapNode.(*ast.MessageNode); ok {
							lg.Debug("special case: hovering over map field name, and map node is a message node")
							return fd.Messages().ByName(protoreflect.Name(msgNode.Name.Val)).Fields().ByName(protoreflect.Name(field.Name.Val)), nil, nil
						}
					case field.MapType.KeyType.Val:
						// hovering over the map key type - do nothing, this is not a special case
						lg.Debug("special case: hovering over map key type")
					case string(field.MapType.ValueType.AsIdentifier()):
						// hovering over the map value type, so return the value type
						lg.Debug("special case: hovering over map value type: " + string(field.MapType.ValueType.AsIdentifier()) + " / " + descriptor.Field[1].GetTypeName())
						definitionFullName = protoreflect.FullName(descriptor.Field[1].GetTypeName())
					}
				case *ast.CompoundIdentNode:
					switch ident.Val {
					case field.Name.Val:
						// ???
						lg.Debug("special case: ???")
					case field.MapType.KeyType.Val:
						// hovering over the map key type - do nothing, this is not a special case
						lg.Debug("special case: hovering over compound map key type")
					case string(field.MapType.ValueType.AsIdentifier()):
						// hovering over the map value type, so return the value type
						definitionFullName = protoreflect.FullName(field.MapType.ValueType.AsIdentifier())
						lg.Debug("special case: hovering over compound map value type: " + string(definitionFullName))
					default:
						return nil, nil, fmt.Errorf("unexpected compound map field node type %T", ident)
					}
				default:
					return nil, nil, fmt.Errorf("unexpected map field node type %T", ident)
				}
			case *ast.MessageNode:
				return fd.Messages().ByName(protoreflect.Name(descriptor.GetName())), nil, nil
			}
		case *descriptorpb.EnumDescriptorProto:
			return fd.Enums().ByName(protoreflect.Name(descriptor.GetName())), nil, nil
		case *descriptorpb.EnumValueDescriptorProto:
			// go up one more level
			enumNode := path[closestIdentifiableIndex-1]
			if enumNode, ok := enumNode.(*ast.EnumNode); ok {
				return fd.Enums().ByName(protoreflect.Name(enumNode.Name.Val)).Values().ByName(protoreflect.Name(descriptor.GetName())), nil, nil
			}
		case *descriptorpb.ServiceDescriptorProto:
			return fd.Services().ByName(protoreflect.Name(descriptor.GetName())), nil, nil
		case *descriptorpb.MethodDescriptorProto:
			// case 1: [Compound]Ident <- *ast.RPCNode = hovering over the method
			// case 2: [Compound]Ident <- *ast.RPCTypeNode <- *ast.RPCNode = hovering over the input or output type
			rpcNode, ok := path[closestIdentifiableIndex].(*ast.RPCNode)
			if !ok {
				break // ???
			}
			switch path[closestIdentifiableIndex+1].(type) {
			case *ast.RPCTypeNode:
				// case 2
				var val string
				switch ident := path[closestIdentifiableIndex+2].(type) {
				case *ast.IdentNode:
					val = ident.Val
				case *ast.CompoundIdentNode:
					val = ident.Val
				}

				if string(rpcNode.Input.MessageType.AsIdentifier()) == val {
					definitionFullName = protoreflect.FullName(descriptor.GetInputType())
				} else if string(rpcNode.Output.MessageType.AsIdentifier()) == val {
					definitionFullName = protoreflect.FullName(descriptor.GetOutputType())
				}
			case *ast.IdentNode, *ast.CompoundIdentNode:
				// case 1
				rpcNode := path[closestIdentifiableIndex-1]
				if svcNode, ok := rpcNode.(*ast.ServiceNode); ok {
					rng := toRange(fileNode.NodeInfo(svcNode.Name))
					return fd.Services().ByName(protoreflect.Name(svcNode.Name.AsIdentifier())).Methods().ByName(protoreflect.Name(descriptor.GetName())), &rng, nil
				}
			}

		// 3. Fields
		case *descriptorpb.FieldDescriptorProto:
			// 3.1:  cursor is over the field name, not the type
			lg.Debug("special case: hovering over field name")
			var val string
			switch ident := path[len(path)-1].(type) {
			case *ast.IdentNode:
				val = ident.Val
			}
			if val == descriptor.GetName() {
				lg.Debug("special case: hovering over field name; field name matches")
				// go up one more level
				msgNode := path[closestIdentifiableIndex-1]
				switch msgNode := msgNode.(type) {
				case *ast.MessageNode:
					lg.Debug("special case: hovering over field name; field name matches; msgNode is *ast.MessageNode")
					if field := fd.Messages().ByName(protoreflect.Name(msgNode.Name.AsIdentifier())).Fields().ByName(protoreflect.Name(descriptor.GetName())); field != nil {
						lg.Debug("special case: hovering over field name; field name matches; msgNode is *ast.MessageNode; field is not nil")
						return field, nil, nil
					}
				}
			}
			lg.Debug("special case: hovering over field name; field name does not match")

			// 3.2: the field type is compound, and the cursor is over the package segment, not the message type name
			if compound, ok := path[len(path)-2].(*ast.CompoundIdentNode); ok {
				components := compound.Components
				if val != components[len(components)-1].Val {
					lg.Debug("special case: hovering over field type package segment")
					// assume the package name is all the parts except the last one
					pkgNameSegments := []string{}
					for _, component := range components[:len(components)-1] {
						pkgNameSegments = append(pkgNameSegments, component.Val)
					}
					pkgName := strings.Join(pkgNameSegments, ".")
					// find the import that matches the package name
					imports := fd.Imports()
					for i := 0; i < imports.Len(); i++ {
						imp := imports.Get(i)
						if imp.Package() == protoreflect.FullName(pkgName) {
							lg.Debug("special case: hovering over field type package segment; package name matches; import found")
							start := fileNode.NodeInfo(components[0])
							end := fileNode.NodeInfo(components[len(components)-2])
							rng := positionsToRange(start.Start(), end.End())
							return imp, &rng, nil
						}
					}
				}
			}
			lg.Debug("special case: hovering over field type package segment; package name does not match")
		}
	}

	switch desc := descriptor.(type) {
	case *descriptorpb.FieldDescriptorProto:
		if dotPrefixedFqn := desc.GetTypeName(); len(dotPrefixedFqn) > 0 && dotPrefixedFqn[0] == '.' {
			fqn := protoreflect.FullName(dotPrefixedFqn[1:])
			if !fqn.IsValid() {
				return nil, nil, fmt.Errorf("%q is not a valid full name", fqn)
			}
			definitionFullName = fqn
		}
	case *descriptorpb.MethodDescriptorProto:
		rpcNode := path[closestIdentifiableIndex]
		switch rpcNode := rpcNode.(type) {
		case *ast.RPCNode:
			rpcTypeNode := path[closestIdentifiableIndex+1]
			switch rpcTypeNode := rpcTypeNode.(type) {
			case *ast.RPCTypeNode:
				var fqn string
				if rpcNode.Input == rpcTypeNode {
					fqn = desc.GetInputType()
				} else {
					fqn = desc.GetOutputType()
				}
				if len(fqn) > 0 && fqn[0] == '.' {
					fqn = fqn[1:]
				}
				if !protoreflect.FullName(fqn).IsValid() {
					return nil, nil, fmt.Errorf("%q is not a valid full name", fqn)
				}
				definitionFullName = protoreflect.FullName(fqn)
			default:
				return nil, nil, fmt.Errorf("unexpected node type %T", rpcTypeNode)
			}
		default:
			return nil, nil, fmt.Errorf("unexpected node type %T", rpcNode)
		}
	case *descriptorpb.FileDescriptorProto:
		fd, err := snap.Files.AsResolver().FindFileByPath(desc.GetName())
		if err != nil || fd == nil {
			return nil, nil, fmt.Errorf("failed to find file by path %q: %w", desc.GetName(), err)
		}
		return fd, nil, nil
	case *descriptorpb.UninterpretedOption_NamePart:
		namePart := desc.GetNamePart()
		if len(namePart) > 0 && namePart[0] == '.' {
			fqn := protoreflect.FullName(namePart[1:])
			if !fqn.IsValid() {
				return nil, nil, fmt.Errorf("%q is not a valid full name", fqn)
			}
			definitionFullName = fqn
		}
		if desc.GetIsExtension() {
			extType, err := snap.Files.AsResolver().FindExtensionByName(definitionFullName)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to find extension by name %q: %w", definitionFullName, err)
			}
			definitionFullName = extType.TypeDescriptor().FullName()
		}
	case *descriptorpb.UninterpretedOption:
		for _, part := range desc.GetName() {
			if part.GetIsExtension() {
				extType, err := snap.Files.AsResolver().FindExtensionByName(protoreflect.FullName(part.GetNamePart()[1:]))
				if err != nil {
					return nil, nil, fmt.Errorf("failed to find extension by name %q: %w", definitionFullName, err)
				}
				definitionFullName = extType.TypeDescriptor().FullName()
				break
			}
		}
		// default:
		// 	return nil, fmt.Errorf("unimplemented descriptor type %T", desc)
	}

	desc, err := snap.Files.AsResolver().FindDescriptorByName(definitionFullName)
	if err != nil {
		if msg, err := protoregistry.GlobalTypes.FindMessageByName(definitionFullName); err == nil {
			desc = msg.Descriptor()
		} else if msg, err := protoregistry.GlobalTypes.FindEnumByName(definitionFullName); err == nil {
			desc = msg.Descriptor()
		} else if msg, err := protoregistry.GlobalTypes.FindExtensionByName(definitionFullName); err == nil {
			desc = msg.TypeDescriptor()
		} else {
			return nil, nil, fmt.Errorf("failed to find descriptor for %q: %w", definitionFullName, err)
		}
	}

	return desc, nil, nil
}
