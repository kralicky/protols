package lsp

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/kralicky/protocompile/ast"
	"github.com/kralicky/protocompile/linker"
	"github.com/kralicky/protocompile/parser"
	"github.com/kralicky/protocompile/protoutil"
	"github.com/kralicky/tools-lite/gopls/pkg/protocol"
	"google.golang.org/protobuf/reflect/protopath"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

var ErrNoDescriptorFound = fmt.Errorf("failed to find descriptor")

// This file contains various algorithms to search and traverse through an AST
// to locate descriptors and nodes from position info.

// Traverses the given path backwards to find the closest top-level mapped
// descriptor, then traverses forwards to find the deeply nested descriptor
// for the original ast node.
func deepPathSearch(path []ast.Node, parseRes parser.Result, linkRes linker.Result) (protoreflect.Descriptor, protocol.Range, error) {
	root := linkRes.AST()

	if len(path) == 0 {
		panic("bug: empty path")
	}
	if _, ok := path[0].(*ast.FileNode); !ok {
		panic("bug: first path element is not an *ast.FileNode")
	}

	if len(path) == 1 {
		return linkRes, toRange(root.NodeInfo(root)), nil
	}

	stack := stack{}

	for i := len(path) - 1; i > 0; i-- {
		currentNode := path[i]
		switch currentNode.(type) {
		// short-circuit for some nodes that we know don't map to descriptors -
		// keywords and numbers
		case *ast.SyntaxNode,
			*ast.PackageNode,
			*ast.EmptyDeclNode,
			*ast.RuneNode,
			*ast.UintLiteralNode,
			*ast.NegativeIntLiteralNode,
			*ast.FloatLiteralNode,
			*ast.SpecialFloatLiteralNode,
			*ast.SignedFloatLiteralNode,
			*ast.StringLiteralNode, *ast.CompoundStringLiteralNode: // TODO: this could change in the future
			return nil, protocol.Range{}, nil
		}
		nodeDescriptor := parseRes.Descriptor(currentNode)
		if nodeDescriptor == nil {
			// this node does not directly map to a descriptor. push it on the stack
			// and go up one level
			stack.push(currentNode, nil)
		} else {
			// this node does directly map to a descriptor.
			var desc protoreflect.Descriptor
			switch nodeDescriptor := nodeDescriptor.(type) {
			case *descriptorpb.FileDescriptorProto:
				desc = linkRes.ParentFile()
			case *descriptorpb.DescriptorProto:
				var typeName string
				// check if it's a synthetic map field
				isMapEntry := nodeDescriptor.GetOptions().GetMapEntry()
				if isMapEntry {
					// if it is, we're looking for the value message, but only if the
					// location is within the value token and not the key token.
					// look ahead two path entries to check which token we're in
					if len(path) > i+2 {
						if mapTypeNode, ok := path[i+1].(*ast.MapTypeNode); ok {
							if identNode, ok := path[i+2].(ast.AnyIdentValueNode); ok {
								if identNode == mapTypeNode.KeyType {
									return nil, protocol.Range{}, nil
								}
							}
						}
					}
					typeName = strings.TrimPrefix(nodeDescriptor.Field[1].GetTypeName(), ".")
				} else {
					typeName = nodeDescriptor.GetName()
				}
				// check if we're looking for a nested message
				prevIndex := i - 1
				if isMapEntry {
					prevIndex-- // go up one more level, we're inside a map field node
				}
				if prevIndex >= 0 {
					if _, ok := path[prevIndex].(*ast.MessageNode); ok {
						// the immediate parent is another message, so this message is not
						// a top-level descriptor. push it on the stack and go up one level
						stack.push(currentNode, nil)
						continue
					}
				}
				// search top-level messages
				desc = linkRes.Messages().ByName(protoreflect.Name(typeName))

				if desc == nil && isMapEntry {
					// the message we are looking for is somewhere nested within the
					// current top-level message, but is not a top-level message itself.
					stack.push(currentNode, nil)
					continue
				}
			case *descriptorpb.EnumDescriptorProto:
				// check if we're looking for an enum nested in a message
				// (enums can't be nested in other enums)
				if i > 0 {
					if _, ok := path[i-1].(*ast.MessageNode); ok {
						// the immediate parent is a message, so this enum is not
						// a top-level descriptor. push it on the stack and go up one level
						stack.push(currentNode, nil)
						continue
					}
				}
				desc = linkRes.Enums().ByName(protoreflect.Name(nodeDescriptor.GetName()))
			case *descriptorpb.ServiceDescriptorProto:
				desc = linkRes.Services().ByName(protoreflect.Name(nodeDescriptor.GetName()))
			case *descriptorpb.UninterpretedOption_NamePart:
				desc = linkRes.FindOptionNameFieldDescriptor(nodeDescriptor)
			case *descriptorpb.UninterpretedOption:
				field := linkRes.FindOptionFieldDescriptor(nodeDescriptor)
				if field != nil {
					switch field.Kind() {
					case protoreflect.MessageKind:
						desc = field.Message()
					case protoreflect.EnumKind:
						desc = field.Enum()
					default:
						return nil, protocol.Range{}, fmt.Errorf("option value is a scalar type (%s)", field.Kind())
					}
				}
			default:
				// not a top-level descriptor. push it on the stack and go up one level
				stack.push(currentNode, nil)
				continue
			}
			if desc == nil {
				switch nodeDescriptor := nodeDescriptor.(type) {
				case *descriptorpb.UninterpretedOption_NamePart:
					switch nodeDescriptor.GetNamePart() {
					case "default", "json_name":
						return nil, protocol.Range{}, fmt.Errorf("option %q is a language builtin", nodeDescriptor.GetNamePart())
					}
				}
				return nil, protocol.Range{}, fmt.Errorf("could not find descriptor for %T", nodeDescriptor)
			}
			stack.push(currentNode, desc)
			break
		}
	}

	// fast path: the node is directly mapped to a resolved top-level descriptor
	if len(stack) == 1 && stack[0].desc != nil {
		return stack[0].desc, toRange(root.NodeInfo(stack[0].node)), nil
	}

	stack.push(path[0], linkRes)

	for i := len(stack) - 1; i >= 0; i-- {
		want := stack[i]
		if want.isResolved() {
			continue
		}
		have := want.nextResolved()
		switch haveDesc := have.desc.(type) {
		case protoreflect.FileDescriptor:
			switch wantNode := want.node.(type) {
			case ast.AnyFileElement:
				switch wantNode := wantNode.(type) {
				case *ast.OptionNode:
					want.desc = haveDesc.Options().(*descriptorpb.FileOptions).ProtoReflect().Descriptor()
				case *ast.ImportNode:
					if wantNode.IsIncomplete() {
						return nil, protocol.Range{}, nil
					}
					wantName := wantNode.Name.AsString()
					imports := haveDesc.Imports()
					for i, l := 0, imports.Len(); i < l; i++ {
						imp := imports.Get(i)
						if imp.Path() == wantName {
							want.desc = imp
							break
						}
					}
				case *ast.MessageNode:
					want.desc = haveDesc.Messages().ByName(wantNode.Name.AsIdentifier())
				case *ast.EnumNode:
					want.desc = haveDesc.Enums().ByName(wantNode.Name.AsIdentifier())
				case *ast.ExtendNode:
					want.desc = haveDesc
				case *ast.ServiceNode:
					want.desc = haveDesc.Services().ByName(wantNode.Name.AsIdentifier())
				}
			case *ast.FieldNode:
				want.desc = haveDesc.Extensions().ByName(wantNode.Name.AsIdentifier())
			case *ast.CompoundIdentNode:
				switch prevNode := want.prev.node.(type) {
				case *ast.ExtendNode:
					// looking for the extendee in the "extend <extendee> {" statement
					if wantNode.AsIdentifier() == prevNode.Extendee.AsIdentifier() {
						want.desc = linkRes.FindExtendeeDescriptorByName(protoreflect.FullName(strings.TrimPrefix(string(wantNode.AsIdentifier()), ".")))
					}
				}
			case *ast.IdentNode:
				switch prevNode := want.prev.node.(type) {
				case *ast.ExtendNode:
					// looking for one segment of a compound ident in the extendee in "extend <extendee> {"
					if wantNode.GetToken() >= prevNode.Extendee.Start() && wantNode.GetToken() <= prevNode.Extendee.End() {
						want.desc = linkRes.FindExtendeeDescriptorByName(protoreflect.FullName(strings.TrimPrefix(string(prevNode.Extendee.AsIdentifier()), ".")))
						if want.desc == nil && len(prevNode.Decls) == 0 {
							// this extend node is not technically valid yet, and is not
							// linked to a descriptor.
							return nil, protocol.Range{}, fmt.Errorf("extend declaration is invalid")
						}
					}
				}
			case *ast.StringLiteralNode:
				if fd, ok := have.desc.(protoreflect.FileImport); ok {
					if fd.FileDescriptor == nil {
						// nothing to do
						return nil, protocol.Range{}, nil
					}
					want.desc = fd.FileDescriptor
				}
			}
		case protoreflect.MessageDescriptor:
			switch wantNode := want.node.(type) {
			case ast.AnyMessageElement:
				switch wantNode := wantNode.(type) {
				case *ast.OptionNode:
					want.desc = haveDesc.Options().(*descriptorpb.MessageOptions).ProtoReflect().Descriptor()
				case *ast.FieldNode:
					if _, ok := have.node.(*ast.ExtendNode); ok {
						// (proto2 only) nested extension declaration
						want.desc = haveDesc.Extensions().ByName(wantNode.Name.AsIdentifier())
					} else {
						want.desc = haveDesc.Fields().ByName(wantNode.Name.AsIdentifier())
					}
				case *ast.MapFieldNode:
					want.desc = haveDesc.Fields().ByName(wantNode.Name.AsIdentifier())
				case *ast.OneofNode:
					want.desc = haveDesc.Oneofs().ByName(wantNode.Name.AsIdentifier())
				case *ast.GroupNode:
					want.desc = haveDesc.Messages().ByName(wantNode.Name.AsIdentifier())
				case *ast.MessageNode:
					want.desc = haveDesc.Messages().ByName(wantNode.Name.AsIdentifier())
				case *ast.EnumNode:
					want.desc = haveDesc.Enums().ByName(wantNode.Name.AsIdentifier())
				case *ast.ExtendNode:
					// (proto2 only) looking for a nested extension declaration.
					// can't do anything yet, we need to resolve by field name
					want.desc = haveDesc
				case *ast.ExtensionRangeNode:
				case *ast.ReservedNode:
				}
			case *ast.FieldReferenceNode:
				if wantNode.IsAnyTypeReference() {
					want.desc = linkRes.FindMessageDescriptorByTypeReferenceURLNode(wantNode)
				} else {
					want.desc = haveDesc.Fields().ByName(wantNode.Name.AsIdentifier())
				}
			case *ast.MessageLiteralNode:
				want.desc = haveDesc
			case *ast.MessageFieldNode:
				name := wantNode.Name
				if name.IsAnyTypeReference() {
					want.desc = linkRes.FindMessageDescriptorByTypeReferenceURLNode(name)
				} else if name.IsExtension() {
					// formatted inside square brackets, e.g. {[path.to.extension.name]: value}
					fqn := linkRes.ResolveMessageLiteralExtensionName(wantNode.Name.Name)
					if fqn == "" {
						return nil, protocol.Range{}, fmt.Errorf("failed to resolve extension name %q", wantNode.Name.Name)
					}
					want.desc = linkRes.FindDescriptorByName(protoreflect.FullName(fqn[1:])).(protoreflect.ExtensionDescriptor)
				} else {
					want.desc = haveDesc.Fields().ByName(protoreflect.Name(wantNode.Name.Value()))
				}
			case ast.AnyIdentValueNode:
				want.desc = haveDesc
			}
		case protoreflect.ExtensionTypeDescriptor:
			switch wantNode := want.node.(type) {
			case ast.AnyIdentValueNode:
				switch containingField := want.prev.node.(type) {
				case *ast.FieldReferenceNode:
					want.desc = haveDesc
				case *ast.FieldNode:
					if wantNode == containingField.Name {
						want.desc = haveDesc.Descriptor()
					} else {
						found := false
						switch ident := containingField.FieldType.Unwrap().(type) {
						case *ast.IdentNode:
							found = wantNode == ident
						case *ast.CompoundIdentNode:
							if wantNode == ident {
								found = true
								break
							}
							for _, comp := range ident.Components {
								if wantNode == comp.GetIdent() {
									found = true
									break
								}
							}
						}
						if found {
							switch haveDesc.Kind() {
							case protoreflect.MessageKind:
								want.desc = haveDesc.Message()
							case protoreflect.EnumKind:
								want.desc = haveDesc.Enum()
							}
						}
					}
				}
			}
		case protoreflect.FieldDescriptor:
			switch wantNode := want.node.(type) {
			case ast.AnyFieldDeclNode:
				want.desc = haveDesc.Message().Fields().ByName(wantNode.GetName().AsIdentifier())
			case *ast.FieldReferenceNode:
				want.desc = haveDesc
			case *ast.ArrayLiteralNode:
				want.desc = haveDesc
			case *ast.MessageLiteralNode:
				want.desc = haveDesc.Message()
			case *ast.MessageFieldNode:
				name := wantNode.Name
				if name.IsAnyTypeReference() {
					want.desc = linkRes.FindMessageDescriptorByTypeReferenceURLNode(name)
				} else {
					want.desc = haveDesc.Message().Fields().ByName(protoreflect.Name(wantNode.Name.Value()))
				}
			case *ast.MapTypeNode:
				// If we get here, we passed through a synthetic map type node, which
				// is directly mapped -- we just couldn't detect it earlier since it
				// isn't actually present at the location we're looking at.
				want.desc = haveDesc.MapValue().Message()
			case *ast.CompactOptionsNode:
				want.desc = haveDesc.Options().(*descriptorpb.FieldOptions).ProtoReflect().Descriptor()
			case ast.AnyIdentValueNode:
				// need to disambiguate
				switch haveNode := have.node.(type) {
				case *ast.FieldReferenceNode:
					want.desc = haveDesc
				case *ast.MessageFieldNode:
					switch haveDesc.Kind() {
					case protoreflect.EnumKind:
						switch val := haveNode.Val.Unwrap().(type) {
						case *ast.IdentNode:
							want.desc = haveDesc.Enum().Values().ByName(val.AsIdentifier())
						case *ast.ArrayLiteralNode:
							for _, el := range val.Elements {
								if wantNode == ast.Node(el.Unwrap()) { // FIXME
									want.desc = haveDesc.Enum().Values().ByName(wantNode.AsIdentifier())
								}
							}
						}
					}
				case ast.AnyFieldDeclNode:
					switch {
					case wantNode.Start() >= haveNode.GetFieldTypeNode().Start() && wantNode.End() <= haveNode.GetFieldTypeNode().End():
						switch {
						case haveDesc.IsExtension():
							// keep the field descriptor
						case haveDesc.IsMap():
							want.desc = haveDesc.MapValue()
						case haveDesc.Kind() == protoreflect.MessageKind:
							want.desc = haveDesc.Message()
						case haveDesc.Kind() == protoreflect.EnumKind:
							want.desc = haveDesc.Enum()
						}
					case wantNode == haveNode.GetName():
						// keep the field descriptor
						// this may be nil if we're in a regular message field, but set if
						// we are in a message literal
						if want.desc == nil {
							want.desc = have.desc
						}
					}
				}
			}
		case protoreflect.EnumDescriptor:
			switch wantNode := want.node.(type) {
			case ast.AnyEnumElement:
				switch wantNode := wantNode.(type) {
				case *ast.OptionNode:
					want.desc = haveDesc.Options().(*descriptorpb.EnumOptions).ProtoReflect().Descriptor()
				case *ast.EnumValueNode:
					want.desc = haveDesc.Values().ByName(wantNode.Name.AsIdentifier())
				case *ast.ReservedNode:
				}
			case *ast.IdentNode:
				// this could be either the enum name itself or a value name
				if haveNode, ok := have.node.(*ast.EnumNode); ok && haveNode.Name == wantNode {
					want.desc = haveDesc
				} else {
					want.desc = haveDesc.Values().ByName(wantNode.AsIdentifier())
				}
			}
		case protoreflect.EnumValueDescriptor:
			switch want.node.(type) {
			case *ast.EnumValueNode:
				want.desc = haveDesc
			case *ast.CompactOptionsNode:
				want.desc = haveDesc.Options().(*descriptorpb.EnumValueOptions).ProtoReflect().Descriptor()
			case ast.AnyIdentValueNode:
				want.desc = haveDesc
			}
		case protoreflect.ServiceDescriptor:
			switch wantNode := want.node.(type) {
			case ast.AnyServiceElement:
				switch wantNode := wantNode.(type) {
				case *ast.OptionNode:
					want.desc = haveDesc.Options().(*descriptorpb.ServiceOptions).ProtoReflect().Descriptor()
				case *ast.RPCNode:
					want.desc = haveDesc.Methods().ByName(wantNode.Name.AsIdentifier())
				}
			case ast.AnyIdentValueNode:
				want.desc = haveDesc
			}
		case protoreflect.MethodDescriptor:
			switch wantNode := want.node.(type) {
			case ast.AnyRPCElement:
				switch wantNode.(type) {
				case *ast.OptionNode:
					want.desc = haveDesc.Options().(*descriptorpb.MethodOptions).ProtoReflect().Descriptor()
				default:
				}
			case *ast.RPCTypeNode:
				if haveNode, ok := have.node.(*ast.RPCNode); ok {
					switch want.node {
					case haveNode.Input:
						want.desc = haveDesc.Input()
					case haveNode.Output:
						want.desc = haveDesc.Output()
					}
				}
			case *ast.CompactOptionsNode:
				want.desc = haveDesc.Options().(*descriptorpb.MethodOptions).ProtoReflect().Descriptor()
			case ast.AnyIdentValueNode:
				want.desc = haveDesc
			}
		case protoreflect.OneofDescriptor:
			switch wantNode := want.node.(type) {
			case ast.AnyOneofElement:
				switch wantNode := wantNode.(type) {
				case *ast.OptionNode:
					want.desc = haveDesc.Options().(*descriptorpb.OneofOptions).ProtoReflect().Descriptor()
				case *ast.FieldNode:
					want.desc = haveDesc.Fields().ByName(wantNode.Name.AsIdentifier())
				}
			case ast.AnyIdentValueNode:
				want.desc = haveDesc
			}
		default:
			return nil, protocol.Range{}, fmt.Errorf("unknown descriptor type %T", have.desc)
		}
		if want.desc == nil {
			return nil, protocol.Range{}, fmt.Errorf("%w for %T/%T", ErrNoDescriptorFound, want.desc, want.node)
		}
	}

	if len(stack) == 0 {
		// nothing relevant found
		return nil, protocol.Range{}, nil
	}

	// as a special case, adjust the range for compound identifiers
	if _, ok := stack[0].node.(*ast.IdentNode); ok {
		if compoundIdent, ok := stack[1].node.(*ast.CompoundIdentNode); ok {
			return stack[0].desc, toRange(root.NodeInfo(compoundIdent)), nil
		}
	}

	return stack[0].desc, toRange(root.NodeInfo(stack[0].node)), nil
}

type stackEntry struct {
	node ast.Node
	desc protoreflect.Descriptor
	prev *stackEntry
}

func (s *stackEntry) isResolved() bool {
	return s.desc != nil
}

func (s *stackEntry) nextResolved() *stackEntry {
	res := s
	for {
		if res == nil {
			panic("bug: stackEntry.nextResolved() called with no resolved entry")
		}
		if res.isResolved() {
			return res
		}
		res = res.prev
	}
}

type stack []*stackEntry

func (s *stack) push(node ast.Node, desc protoreflect.Descriptor) {
	e := &stackEntry{
		node: node,
		desc: desc,
	}
	if len(*s) > 0 {
		(*s)[len(*s)-1].prev = e
	}
	*s = append(*s, e)
}

// find the narrowest non-rune token that contains the position and also has a node
// associated with it. The set of tokens will contain all the tokens that
// contain the position, scoped to the narrowest top-level declaration (message, service, etc.)
//
//	         [(file)→(message Foo)→(ident 'Foo')]
//	         ↓
//	message F͟o͟o͟ {
//	           [(file)→(message Foo)→(message Bar)→(ident 'Bar')]
//	           ↓
//	  message B͟a͟r͟ {
//	             [(file)→(message Foo)→(message Bar)→(message Baz)→(ident 'Baz')]
//	             ↓
//	    message B͟a͟z͟ {      [(file)→(message Foo)→(message Bar)→(message Baz)→(field var)→(ident 'var')]
//	                       ↓
//	      optional string v͟a͟r͟ = 1;
//
//	       [(file)→(message Foo)→(message Bar)→(message Baz)→(field var2)→(ident 'Bar')]
//	       ↓
//	      B͟a͟r͟ var2 = 2 [option = {v͟a͟l͟u͟e͟: 5}];
//	                               ↑
//	                               [(file)→(message Foo)→(message Bar)→(message Baz)→(field var2)→
//	                                (compact options)→(message literal)→(field reference)→(ident 'value')]
//	    }
//	  }
//	}
func findNarrowestSemanticToken(parseRes parser.Result, tokens []semanticItem, pos protocol.Position) (narrowest semanticItem, found bool) {
	for _, token := range tokens {
		if token.lang != tokenLanguageProto {
			// ignore non-proto tokens
			continue
		}
		if pos.Line != token.line {
			if token.line > pos.Line {
				// Stop searching once we've passed the line
				break
			}
			continue // Skip tokens not on the same line
		}
		if token.len == 0 {
			// Skip tokens with no length
			continue
		}
		// Note: this allows the cursor to be at the end of a token, which is
		// consistent with observed gopls behavior.
		if token.start+token.len < pos.Character {
			// Skip tokens that end before the position
			continue
		}
		if token.start > pos.Character {
			// Stop searching once we've passed the position
			break
		}
		if token.node == nil {
			// Skip tokens without a node
			continue
		}
		if _, ok := token.node.(*ast.RuneNode); ok {
			// Skip rune nodes (parens, separators, brackets, etc)
			continue
		}
		return token, true
	}

	return
}

// find the narrowest non-terminal non-ident scope that contains the position and also has a node
// associated with it. The set of tokens will contain all the tokens that contain the position,
// scoped to the narrowest top-level declaration (message, service, etc.).
// The last entry in the path is limited to the following ast node types:
//
//	MessageNode:
//	message Foo {
//	  __ <- [(file)→(message Foo)]
//	}
//
//	OptionNameNode:
//	message Foo {
//	  option (f͟o͟o͟) = true;
//	           ↑
//	           [(file)→(message Foo)→(option)→(option name)]
//	  option (foo).bar.b͟a͟z͟ = true;
//	                    ↑
//	                    [(file)→(message Foo)→(option)→(option name)]
//	}
//
//	MessageFieldNode:
//	message Foo {
//	  string bar = 1 [
//	    msg: {
//	      key: value
//	      __ <- [(file)→(message Foo)→(field bar)→(compact options)→(msglit msg)]
//	    },
//	  ];
//	}
//
//	CompactOptionsNode:
//	message Foo {
//	  string bar = 1 [
//	    __ <- [(file)→(message Foo)→(field bar)→(compact options)]
//	  ];
//	}
func findPathIntersectingToken(parseRes parser.Result, tokenAtOffset ast.Token, location protocol.Position) ([]ast.Node, protopath.Path, bool) {
	tracker := &ast.AncestorTracker{}
	nodePaths := [][]ast.Node{}
	paths := []protopath.Path{}
	fileNode := parseRes.AST()
	intersectsLocation := func(node ast.Node) bool {
		info := fileNode.NodeInfo(node)
		return protocol.Intersect(toRange(info), protocol.Range{Start: location, End: location})
	}
	intersectsLocationExclusive := func(node, end ast.Node) bool {
		if ast.IsNil(end) {
			return intersectsLocation(node)
		}
		if rn, ok := end.(*ast.RuneNode); ok && rn.Virtual {
			return intersectsLocation(node)
		}
		nodeInfo := fileNode.NodeInfo(node)
		endSourcePos := fileNode.NodeInfo(end).End()
		if protocol.Intersect(positionsToRange(nodeInfo.Start(), endSourcePos), protocol.Range{Start: location, End: location}) {
			return int(location.Line) < endSourcePos.Line-1 || int(location.Character) < endSourcePos.Col-1
		}
		return false
	}
	opts := tracker.AsWalkOptions()
	if tokenAtOffset != ast.TokenError {
		opts = append(opts, ast.WithIntersection(tokenAtOffset))
	}
	appendPaths := func() {
		nodePaths = append(nodePaths, slices.Clone(tracker.Path()))
		paths = append(paths, slices.Clone(tracker.ProtoPath()))
	}
	ast.Inspect(parseRes.AST(), func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.ImportNode:
			if intersectsLocationExclusive(node, node.Semicolon) {
				appendPaths()
			}
		case *ast.SyntaxNode:
			if intersectsLocationExclusive(node, node.Semicolon) {
				appendPaths()
			}
		case *ast.MessageNode:
			if intersectsLocationExclusive(node, node.CloseBrace) {
				appendPaths()
			}
		case *ast.EnumNode:
			if intersectsLocationExclusive(node, node.CloseBrace) {
				appendPaths()
			}
		case *ast.EnumValueNode:
			if intersectsLocationExclusive(node, node.Semicolon) {
				appendPaths()
			}
		case *ast.ServiceNode:
			if intersectsLocationExclusive(node, node.CloseBrace) {
				appendPaths()
			}
		case *ast.RPCNode:
			var end ast.Node
			if node.Semicolon != nil {
				end = node.Semicolon
			} else if node.CloseBrace != nil {
				end = node.CloseBrace
			} else {
				break
			}
			if node.Semicolon != nil && intersectsLocationExclusive(node, end) {
				appendPaths()
			}
		case *ast.ExtendNode:
			if node.IsIncomplete() {
				if intersectsLocation(node) {
					appendPaths()
				}
			} else if intersectsLocationExclusive(node, node.CloseBrace) {
				appendPaths()
			}
		case *ast.OptionNode:
			if intersectsLocationExclusive(node, node.Semicolon) {
				appendPaths()
			}
		case *ast.MessageLiteralNode:
			if intersectsLocationExclusive(node, node.Close) {
				appendPaths()
			}
		case *ast.OptionNameNode:
			if intersectsLocation(node) {
				appendPaths()
			}
		case *ast.MessageFieldNode:
			if intersectsLocation(node) {
				appendPaths()
			}
			if node.Sep != nil && node.Name != nil && tokenAtOffset == node.Sep.GetToken() {
				// this won't be visited by the walker, but we want the path to
				// end with the field reference node if the cursor is between the
				// field name and the separator
				nodePaths = append(nodePaths, append(slices.Clone(tracker.Path()), node.Name))
				paths = append(paths, append(slices.Clone(tracker.ProtoPath()), protopath.FieldAccess(node.ProtoReflect().Descriptor().Fields().ByNumber(1))))
			}
		case *ast.CompactOptionsNode:
			if intersectsLocationExclusive(node, node.CloseBracket) {
				appendPaths()
			}
		case *ast.FieldNode:
			if intersectsLocationExclusive(node, node.Semicolon) {
				appendPaths()
			}
		case *ast.FieldReferenceNode:
			if intersectsLocation(node) {
				appendPaths()
			}
		case *ast.RPCTypeNode:
			if intersectsLocationExclusive(node, node.CloseParen) {
				appendPaths()
			}
		case *ast.PackageNode:
			if intersectsLocationExclusive(node, node.Semicolon) {
				appendPaths()
			}
		case *ast.ErrorNode:
			if intersectsLocation(node) {
				appendPaths()
			}
		}
		return true
	}, opts...)
	if len(nodePaths) == 0 {
		return nil, protopath.Path{}, false
	}

	// take the longest path
	longestPathIdx := 0
	for i, path := range nodePaths {
		if len(path) > len(nodePaths[longestPathIdx]) {
			longestPathIdx = i
		}
	}
	return nodePaths[longestPathIdx], paths[longestPathIdx], true
}

func visitEnclosingRange(tracker *ast.AncestorTracker, paths *[][]ast.Node) bool {
	p := slices.Clone(tracker.Path())
	if len(*paths) == 0 {
		*paths = append(*paths, p)
	} else if len(p) >= len((*paths)[len(*paths)-1]) {
		*paths = append(*paths, p)
	}
	return true
}

func DefaultEnclosingRangeVisitor(tracker *ast.AncestorTracker, paths *[][]ast.Node) func(ast.Node) bool {
	return func(node ast.Node) bool {
		switch node.(type) {
		case *ast.ImportNode,
			*ast.SyntaxNode,
			*ast.MessageNode,
			*ast.EnumNode,
			*ast.EnumValueNode,
			*ast.ServiceNode,
			*ast.RPCNode,
			*ast.ExtendNode,
			*ast.OptionNode,
			*ast.MessageLiteralNode,
			*ast.OptionNameNode,
			*ast.MessageFieldNode,
			*ast.CompactOptionsNode,
			*ast.FieldNode,
			*ast.FieldReferenceNode,
			*ast.RPCTypeNode,
			*ast.PackageNode,
			*ast.ErrorNode:
			return visitEnclosingRange(tracker, paths)
		default:
			return false
		}
	}
}

func findPathsEnclosingRange(parseRes parser.Result, start, end ast.Token, visitor ...func(tracker *ast.AncestorTracker, paths *[][]ast.Node) func(ast.Node) bool) ([][]ast.Node, bool) {
	tracker := &ast.AncestorTracker{}
	opts := tracker.AsWalkOptions()
	opts = append(opts, ast.WithRange(start, end))
	var paths [][]ast.Node

	if len(visitor) == 0 {
		visitor = append(visitor, DefaultEnclosingRangeVisitor)
	}
	ast.Inspect(parseRes.AST(), visitor[0](tracker, &paths), opts...)
	if len(paths) == 0 {
		return nil, false
	}

	lowerBound := len(paths) - 1
	for i := len(paths) - 2; i >= 0; i-- {
		if len(paths[i]) < len(paths[lowerBound]) {
			break
		}
		lowerBound = i
	}
	return paths[lowerBound:], true
}

func findDefinition(desc protoreflect.Descriptor, linkRes linker.Result) (ast.NodeReference, error) {
	var node ast.Node
	switch desc := desc.(type) {
	case protoreflect.MessageDescriptor:
		node = linkRes.MessageNode(desc.(protoutil.DescriptorProtoWrapper).AsProto().(*descriptorpb.DescriptorProto)).GetName()
	case protoreflect.EnumDescriptor:
		node = linkRes.EnumNode(desc.(protoutil.DescriptorProtoWrapper).AsProto().(*descriptorpb.EnumDescriptorProto)).GetName()
	case protoreflect.ServiceDescriptor:
		node = linkRes.ServiceNode(desc.(protoutil.DescriptorProtoWrapper).AsProto().(*descriptorpb.ServiceDescriptorProto)).GetName()
	case protoreflect.MethodDescriptor:
		node = linkRes.MethodNode(desc.(protoutil.DescriptorProtoWrapper).AsProto().(*descriptorpb.MethodDescriptorProto)).GetName()
	case protoreflect.FieldDescriptor:
		if !desc.IsExtension() {
			switch desc := desc.(type) {
			case protoutil.DescriptorProtoWrapper:
				node = linkRes.FieldNode(desc.AsProto().(*descriptorpb.FieldDescriptorProto))
			default:
				// these can be internal filedesc.Field descriptors for e.g. builtin options
				node = linkRes.FieldNode(linkRes.FindDescriptorByName(desc.FullName()).(protoutil.DescriptorProtoWrapper).AsProto().(*descriptorpb.FieldDescriptorProto))
			}
		} else {
			switch desc := desc.(type) {
			case protoutil.DescriptorProtoWrapper:
				node = linkRes.FieldNode(desc.AsProto().(*descriptorpb.FieldDescriptorProto))
			case protoreflect.ExtensionTypeDescriptor:
				node = linkRes.FieldNode(desc.Descriptor().(protoutil.DescriptorProtoWrapper).AsProto().(*descriptorpb.FieldDescriptorProto))
			}
		}
	case protoreflect.EnumValueDescriptor:
		// TODO(editions): builtin enums aren't wrappers here yet
		node = linkRes.EnumValueNode(desc.(protoutil.DescriptorProtoWrapper).AsProto().(*descriptorpb.EnumValueDescriptorProto)).GetName()
	case protoreflect.OneofDescriptor:
		node = linkRes.OneofNode(desc.(protoutil.DescriptorProtoWrapper).AsProto().(*descriptorpb.OneofDescriptorProto))
	case protoreflect.FileDescriptor:
		node = linkRes.FileNode()
		slog.Debug("definition is an import: ", "import", linkRes.Path())
	default:
		return ast.NodeReference{}, fmt.Errorf("unexpected descriptor type %T", desc)
	}
	if node == nil {
		return ast.NodeReference{}, fmt.Errorf("failed to find node for %q", desc.FullName())
	}
	if _, ok := node.(*ast.NoSourceNode); ok {
		return ast.NodeReference{}, fmt.Errorf("no source available")
	}
	return ast.NewNodeReference(linkRes.AST(), node), nil
}

func findNodeReferences(desc protoreflect.Descriptor, files linker.Files) <-chan ast.NodeReference {
	var wg sync.WaitGroup
	refs := make(chan ast.NodeReference, len(files))
	seen := sync.Map{}
	for _, res := range files {
		if res.IsPlaceholder() {
			continue
		}
		wg.Add(1)
		res := res.(linker.Result)
		go func() {
			defer wg.Done()
			for _, ref := range res.FindReferences(desc) {
				if _, seen := seen.LoadOrStore(ref.String(), struct{}{}); !seen {
					refs <- ref
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(refs)
	}()
	return refs
}
