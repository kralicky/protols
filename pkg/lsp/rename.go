package lsp

import (
	"fmt"

	"github.com/kralicky/protocompile/ast"
	"github.com/kralicky/tools-lite/gopls/pkg/lsp/protocol"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func (c *Cache) PrepareRename(in protocol.TextDocumentPositionParams) (*protocol.PrepareRenameResult, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()

	desc, rng, err := c.FindTypeDescriptorAtLocation(in)
	if err != nil {
		return nil, err
	}
	if desc == nil {
		return nil, fmt.Errorf("no type found at location")
	}
	// check if desc can be renamed
	if err := c.canRename(desc); err != nil {
		return nil, err
	}

	return &protocol.PrepareRenameResult{
		Range:       rng,
		Placeholder: string(desc.Name()),
	}, nil
}

func (c *Cache) canRename(desc protoreflect.Descriptor) error {
	// A descriptor can be renamed if:
	// 1. If it is a message, enum, service, or field
	// 2. It is defined in a file that exists on disk in the workspace
	// 3. It is not a synthetic descriptor (e.g. a map entry)

	switch desc.(type) {
	case protoreflect.FileDescriptor:
		return fmt.Errorf("cannot rename file descriptors")
	case protoreflect.MessageDescriptor:
	case protoreflect.FieldDescriptor:
	case protoreflect.OneofDescriptor:
	case protoreflect.EnumDescriptor:
	case protoreflect.EnumValueDescriptor:
	case protoreflect.ServiceDescriptor:
	case protoreflect.MethodDescriptor:
	default:
		return fmt.Errorf("cannot rename %T", desc)
	}

	var definition protocol.Location

	definition, err := c.FindDefinitionForTypeDescriptor(desc)
	if err != nil {
		return fmt.Errorf("failed to find definition for %q: %w", desc.FullName(), err)
	}

	// check if the file is local to this workspace
	uri := definition.URI
	if !c.resolver.IsRealWorkspaceLocalFile(uri) {
		return fmt.Errorf("symbol %q is defined externally to this workspace", desc.FullName())
	}
	if ok, _ := c.LatestDocumentContentsWellFormed(uri, false); !ok {
		return fmt.Errorf("source file containing definition for %q has errors", uri)
	}

	return nil
}

func (c *Cache) Rename(params *protocol.RenameParams) (*protocol.WorkspaceEdit, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()

	desc, _, err := c.FindTypeDescriptorAtLocation(protocol.TextDocumentPositionParams{
		TextDocument: params.TextDocument,
		Position:     params.Position,
	})
	if err != nil {
		return nil, err
	}

	// check if desc can be renamed
	if err := c.canRename(desc); err != nil {
		return nil, err
	}

	// check if the new name is valid
	oldFqn := desc.FullName()

	// replace the name portion of the full name with the new name
	newFqn := oldFqn.Parent().Append(protoreflect.Name(params.NewName))
	// check if the new name is valid
	if !newFqn.IsValid() {
		return nil, fmt.Errorf("invalid name %q", newFqn)
	}

	// check if the new name is already taken
	if _, err := c.results.AsResolver().FindDescriptorByName(newFqn); err == nil {
		return nil, fmt.Errorf("a type already exists with name %q", newFqn)
	}

	// find the definition
	definition, err := findDefinition(desc, c.results)
	if err != nil {
		return nil, err
	}

	// find all references
	refs, err := c.FindReferencesForTypeDescriptor(desc)
	if err != nil {
		return nil, err
	}

	editsByDocument := map[protocol.DocumentURI][]protocol.TextEdit{}
	for _, ref := range append(refs, definition) {
		node := ref.Node
		// only edit the short name, not the qualified name (if it is qualified)
		// we can do this adjustment using only the range
		var editRange ast.SourceSpan
		switch node := node.(type) {
		case *ast.IdentNode:
			editRange = ref.NodeInfo
		case *ast.FieldReferenceNode:
			// ensure only the name is replaced
			switch name := node.Name.(type) {
			case *ast.IdentNode:
				editRange = ref.NodeInfo.Internal().ParentFile().NodeInfo(name)
			case *ast.CompoundIdentNode:
				info := ref.NodeInfo.Internal().ParentFile().NodeInfo(name.Components[len(name.Components)-1])
				if info.IsValid() {
					editRange = info
				} else {
					continue
				}
			default:
				continue
			}

		case *ast.MapFieldNode:
			editRange = ref.NodeInfo.Internal().ParentFile().NodeInfo(node.Name)
		case *ast.FieldNode:
			editRange = ref.NodeInfo.Internal().ParentFile().NodeInfo(node.Name)
		case *ast.EnumValueNode:
			editRange = ref.NodeInfo.Internal().ParentFile().NodeInfo(node.Name)
		case *ast.OneofNode:
			editRange = ref.NodeInfo.Internal().ParentFile().NodeInfo(node.Name)
		case *ast.MessageNode:
			editRange = ref.NodeInfo.Internal().ParentFile().NodeInfo(node.Name)
		case *ast.EnumNode:
			editRange = ref.NodeInfo.Internal().ParentFile().NodeInfo(node.Name)
		case *ast.ServiceNode:
			editRange = ref.NodeInfo.Internal().ParentFile().NodeInfo(node.Name)
		case *ast.RPCNode:
			editRange = ref.NodeInfo.Internal().ParentFile().NodeInfo(node.Name)
		default:
			return nil, fmt.Errorf("cannot rename %T", node)
		}

		uri, err := c.resolver.PathToURI(ref.NodeInfo.Start().Filename)
		if err != nil {
			return nil, err
		}
		if !c.resolver.IsRealWorkspaceLocalFile(uri) {
			return nil, fmt.Errorf("references exist outside of the workspace")
		}
		editsByDocument[uri] = append(editsByDocument[uri], protocol.TextEdit{
			Range:   toRange(editRange),
			NewText: params.NewName,
		})
	}

	// the rename is valid, return the edits to the server
	return &protocol.WorkspaceEdit{
		Changes: editsByDocument,
	}, nil
}
