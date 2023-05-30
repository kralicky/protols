package main

import (
	"errors"
	"fmt"

	"github.com/bufbuild/protocompile/ast"
	protocol "go.lsp.dev/protocol"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
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
				if _, ok := n.(*ast.IdentNode); ok {
					if _, ok := path[len(path)-2].(*ast.CompoundIdentNode); ok {
						path = path[:len(path)-2]
					}
				}
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

func findRelevantDescriptorAtLocation(params *protocol.TextDocumentPositionParams, cache *Cache, lg *zap.Logger) (protoreflect.Descriptor, error) {
	fd, err := cache.FindFileByPath(params.TextDocument.URI.Filename())
	if err != nil {
		return nil, err
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
		return nil, errors.New("no node found at position")
	}

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
		return nil, errors.New("no identifiable node found at position")
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

		// 1. Imports, which resolve to ambiguous file descriptors
		switch descriptor.(type) {
		case *descriptorpb.FileDescriptorProto:
			fileNode := path[closestIdentifiableIndex+1]
			var filename string
			switch fileNode := fileNode.(type) {
			case *ast.ImportNode:
				filename = fileNode.Name.AsString()
			case *ast.StringLiteralNode:
				filename = fileNode.AsString()
			default:
				return nil, fmt.Errorf("unexpected node type %T", fileNode)
			}
			f, err := cache.files.AsResolver().FindFileByPath(filename)
			if err != nil {
				return nil, fmt.Errorf("could not find file %q: %w", filename, err)
			}
			descriptor = protodesc.ToFileDescriptorProto(f)
		}
	}

	switch desc := descriptor.(type) {
	case *descriptorpb.FieldDescriptorProto:
		if dotPrefixedFqn := desc.GetTypeName(); len(dotPrefixedFqn) > 0 && dotPrefixedFqn[0] == '.' {
			fqn := protoreflect.FullName(dotPrefixedFqn[1:])
			if !fqn.IsValid() {
				return nil, fmt.Errorf("%q is not a valid full name", fqn)
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
					return nil, fmt.Errorf("%q is not a valid full name", fqn)
				}
				definitionFullName = protoreflect.FullName(fqn)
			default:
				return nil, fmt.Errorf("unexpected node type %T", rpcTypeNode)
			}
		default:
			return nil, fmt.Errorf("unexpected node type %T", rpcNode)
		}
	case *descriptorpb.FileDescriptorProto:
		fd, err := cache.files.AsResolver().FindFileByPath(desc.GetName())
		if err != nil || fd == nil {
			return nil, fmt.Errorf("failed to find file by path %q: %w", desc.GetName(), err)
		}
		return fd, nil
	case *descriptorpb.UninterpretedOption_NamePart:
		namePart := desc.GetNamePart()
		if len(namePart) > 0 && namePart[0] == '.' {
			fqn := protoreflect.FullName(namePart[1:])
			if !fqn.IsValid() {
				return nil, fmt.Errorf("%q is not a valid full name", fqn)
			}
			definitionFullName = fqn
		}
		if desc.GetIsExtension() {
			extType, err := cache.files.AsResolver().FindExtensionByName(definitionFullName)
			if err != nil {
				return nil, fmt.Errorf("failed to find extension by name %q: %w", definitionFullName, err)
			}
			definitionFullName = extType.TypeDescriptor().FullName()
		}
	case *descriptorpb.UninterpretedOption:
		for _, part := range desc.GetName() {
			if part.GetIsExtension() {
				extType, err := cache.files.AsResolver().FindExtensionByName(protoreflect.FullName(part.GetNamePart()[1:]))
				if err != nil {
					return nil, fmt.Errorf("failed to find extension by name %q: %w", definitionFullName, err)
				}
				definitionFullName = extType.TypeDescriptor().FullName()
				break
			}
		}

	default:
		return nil, fmt.Errorf("unimplemented descriptor type %T", desc)
	}

	desc, err := cache.files.AsResolver().FindDescriptorByName(definitionFullName)
	if err != nil {
		return nil, fmt.Errorf("failed to find descriptor for %q: %w", definitionFullName, err)
	}

	return desc, nil
}
