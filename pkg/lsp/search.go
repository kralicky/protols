package lsp

import (
	"cmp"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/kralicky/protocompile/ast"
	"github.com/kralicky/protocompile/linker"
	"github.com/kralicky/protocompile/parser"
	"github.com/kralicky/protocompile/protoutil"
	"github.com/kralicky/tools-lite/gopls/pkg/lsp/protocol"
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
		case *ast.KeywordNode,
			*ast.SyntaxNode,
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
							if identNode, ok := path[i+2].(ast.IdentValueNode); ok {
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
			case ast.FileElement:
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
					want.desc = haveDesc.Messages().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
				case *ast.EnumNode:
					want.desc = haveDesc.Enums().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
				case *ast.ExtendNode:
					want.desc = haveDesc
				case *ast.ServiceNode:
					want.desc = haveDesc.Services().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
				}
			case *ast.FieldNode:
				want.desc = haveDesc.Extensions().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
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
					if wantNode.Token() >= prevNode.Extendee.Start() && wantNode.Token() <= prevNode.Extendee.End() {
						want.desc = linkRes.FindExtendeeDescriptorByName(protoreflect.FullName(strings.TrimPrefix(string(prevNode.Extendee.AsIdentifier()), ".")))
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
			case ast.MessageElement:
				switch wantNode := wantNode.(type) {
				case *ast.OptionNode:
					want.desc = haveDesc.Options().(*descriptorpb.MessageOptions).ProtoReflect().Descriptor()
				case *ast.FieldNode:
					if _, ok := have.node.(*ast.ExtendNode); ok {
						// (proto2 only) nested extension declaration
						want.desc = haveDesc.Extensions().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
					} else {
						want.desc = haveDesc.Fields().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
					}
				case *ast.MapFieldNode:
					want.desc = haveDesc.Fields().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
				case *ast.OneofNode:
					want.desc = haveDesc.Oneofs().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
				case *ast.GroupNode:
					want.desc = haveDesc.Messages().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
				case *ast.MessageNode:
					want.desc = haveDesc.Messages().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
				case *ast.EnumNode:
					want.desc = haveDesc.Enums().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
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
					want.desc = haveDesc.Fields().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
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
					want.desc = linkRes.FindDescriptorByName(protoreflect.FullName(fqn[1:])).(protoreflect.ExtensionDescriptor)
				} else {
					want.desc = haveDesc.Fields().ByName(protoreflect.Name(wantNode.Name.Value()))
				}
			case ast.IdentValueNode:
				want.desc = haveDesc
			}
		case protoreflect.ExtensionTypeDescriptor:
			switch wantNode := want.node.(type) {
			case ast.IdentValueNode:
				switch containingField := want.prev.node.(type) {
				case *ast.FieldReferenceNode:
					want.desc = haveDesc
				case *ast.FieldNode:
					if wantNode == containingField.Name {
						want.desc = haveDesc.Descriptor()
					} else {
						composite, ok := containingField.FldType.(ast.CompositeNode)
						if (ok && slices.Contains(composite.Children(), want.node)) || wantNode == containingField.FldType {
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
			case ast.FieldDeclNode:
				switch wantNode := wantNode.(type) {
				case *ast.FieldNode:
					want.desc = haveDesc.Message().Fields().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
				case *ast.GroupNode:
					want.desc = haveDesc.Message().Fields().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
				case *ast.MapFieldNode:
					want.desc = haveDesc.Message().Fields().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
				case *ast.SyntheticMapField:
					want.desc = haveDesc.Message().Fields().ByName(protoreflect.Name(wantNode.Ident.AsIdentifier()))
				}
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
			case ast.IdentValueNode:
				// need to disambiguate
				switch haveNode := have.node.(type) {
				case *ast.FieldReferenceNode:
					want.desc = haveDesc
				case *ast.MessageFieldNode:
					switch haveDesc.Kind() {
					case protoreflect.EnumKind:
						switch val := haveNode.Val.(type) {
						case ast.IdentValueNode:
							want.desc = haveDesc.Enum().Values().ByName(protoreflect.Name(val.AsIdentifier()))
						case *ast.ArrayLiteralNode:
							for _, el := range val.Elements {
								if wantNode == el {
									want.desc = haveDesc.Enum().Values().ByName(protoreflect.Name(wantNode.AsIdentifier()))
								}
							}
						}
					}
				case ast.FieldDeclNode:
					switch {
					case wantNode.Start() >= haveNode.FieldType().Start() && wantNode.End() <= haveNode.FieldType().End():
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
					case wantNode == haveNode.FieldName():
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
			case ast.EnumElement:
				switch wantNode := wantNode.(type) {
				case *ast.OptionNode:
					want.desc = haveDesc.Options().(*descriptorpb.EnumOptions).ProtoReflect().Descriptor()
				case *ast.EnumValueNode:
					want.desc = haveDesc.Values().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
				case *ast.ReservedNode:
				}
			case ast.IdentValueNode:
				// this could be either the enum name itself or a value name
				if haveNode, ok := have.node.(*ast.EnumNode); ok && haveNode.Name == wantNode {
					want.desc = haveDesc
				} else {
					want.desc = haveDesc.Values().ByName(protoreflect.Name(wantNode.AsIdentifier()))
				}
			}
		case protoreflect.EnumValueDescriptor:
			switch wantNode := want.node.(type) {
			case ast.EnumValueDeclNode:
				switch wantNode.(type) {
				case *ast.EnumValueNode:
					want.desc = haveDesc // ??
				case ast.NoSourceNode:
				}
			case *ast.CompactOptionsNode:
				want.desc = haveDesc.Options().(*descriptorpb.EnumValueOptions).ProtoReflect().Descriptor()
			case ast.IdentValueNode:
				want.desc = haveDesc
			}
		case protoreflect.ServiceDescriptor:
			switch wantNode := want.node.(type) {
			case ast.ServiceElement:
				switch wantNode := wantNode.(type) {
				case *ast.OptionNode:
					want.desc = haveDesc.Options().(*descriptorpb.ServiceOptions).ProtoReflect().Descriptor()
				case *ast.RPCNode:
					want.desc = haveDesc.Methods().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
				}
			case ast.IdentValueNode:
				want.desc = haveDesc
			}
		case protoreflect.MethodDescriptor:
			switch wantNode := want.node.(type) {
			case ast.RPCElement:
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
			case ast.IdentValueNode:
				want.desc = haveDesc
			}
		case protoreflect.OneofDescriptor:
			switch wantNode := want.node.(type) {
			case ast.OneofElement:
				switch wantNode := wantNode.(type) {
				case *ast.OptionNode:
					want.desc = haveDesc.Options().(*descriptorpb.OneofOptions).ProtoReflect().Descriptor()
				case *ast.FieldNode:
					want.desc = haveDesc.Fields().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
				}
			case ast.IdentValueNode:
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

// find the narrowest token that contains the position and also has a node
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
		if token.start+token.len-1 < pos.Character {
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
func findNarrowestEnclosingScope(parseRes parser.Result, tokenAtOffset ast.Token, location protocol.Position) ([]ast.Node, bool) {
	tracker := &ast.AncestorTracker{}
	paths := [][]ast.Node{}
	fileNode := parseRes.AST()
	intersectsLocation := func(node ast.Node) bool {
		info := fileNode.NodeInfo(node)
		return protocol.Intersect(toRange(info), protocol.Range{Start: location, End: location})
	}
	intersectsLocationExclusive := func(node, end ast.Node) bool {
		if reflect.ValueOf(end).IsNil() {
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
	ast.Walk(parseRes.AST(), &ast.SimpleVisitor{
		DoVisitImportNode: func(node *ast.ImportNode) error {
			if intersectsLocationExclusive(node, node.Semicolon) {
				paths = append(paths, slices.Clone(tracker.Path()))
			}
			return nil
		},
		DoVisitSyntaxNode: func(node *ast.SyntaxNode) error {
			if intersectsLocationExclusive(node, node.Semicolon) {
				paths = append(paths, slices.Clone(tracker.Path()))
			}
			return nil
		},
		DoVisitMessageNode: func(node *ast.MessageNode) error {
			if intersectsLocationExclusive(node, node.CloseBrace) {
				paths = append(paths, slices.Clone(tracker.Path()))
			}
			return nil
		},
		DoVisitExtendNode: func(node *ast.ExtendNode) error {
			if node.IsIncomplete() {
				if intersectsLocation(node) {
					paths = append(paths, slices.Clone(tracker.Path()))
				}
			} else if intersectsLocationExclusive(node, node.CloseBrace) {
				paths = append(paths, slices.Clone(tracker.Path()))
			}
			return nil
		},
		DoVisitOptionNode: func(node *ast.OptionNode) error {
			if intersectsLocationExclusive(node, node.Semicolon) {
				paths = append(paths, slices.Clone(tracker.Path()))
			}
			return nil
		},
		DoVisitMessageLiteralNode: func(node *ast.MessageLiteralNode) error {
			if intersectsLocationExclusive(node, node.Close) {
				paths = append(paths, slices.Clone(tracker.Path()))
			}
			return nil
		},
		DoVisitOptionNameNode: func(node *ast.OptionNameNode) error {
			if intersectsLocation(node) {
				paths = append(paths, slices.Clone(tracker.Path()))
			}
			return nil
		},
		DoVisitMessageFieldNode: func(node *ast.MessageFieldNode) error {
			if intersectsLocation(node) {
				paths = append(paths, slices.Clone(tracker.Path()))
			}
			if node.Sep != nil && node.Name != nil && tokenAtOffset == node.Sep.Token() {
				// this won't be visited by the walker, but we want the path to
				// end with the field reference node
				paths = append(paths, append(slices.Clone(tracker.Path()), node.Name))
			}
			return nil
		},
		DoVisitCompactOptionsNode: func(node *ast.CompactOptionsNode) error {
			if intersectsLocationExclusive(node, node.CloseBracket) {
				paths = append(paths, slices.Clone(tracker.Path()))
			}
			return nil
		},
		DoVisitFieldNode: func(node *ast.FieldNode) error {
			if intersectsLocationExclusive(node, node.Semicolon) {
				paths = append(paths, slices.Clone(tracker.Path()))
			}
			return nil
		},
		DoVisitFieldReferenceNode: func(node *ast.FieldReferenceNode) error {
			if intersectsLocation(node) {
				paths = append(paths, slices.Clone(tracker.Path()))
			}
			return nil
		},
		DoVisitRPCTypeNode: func(node *ast.RPCTypeNode) error {
			if intersectsLocationExclusive(node, node.CloseParen) {
				paths = append(paths, slices.Clone(tracker.Path()))
			}
			return nil
		},
		DoVisitPackageNode: func(node *ast.PackageNode) error {
			if intersectsLocationExclusive(node, node.Semicolon) {
				paths = append(paths, slices.Clone(tracker.Path()))
			}
			return nil
		},
		DoVisitErrorNode: func(en *ast.ErrorNode) error {
			if intersectsLocation(en) {
				paths = append(paths, slices.Clone(tracker.Path()))
			}
			return nil
		},
	}, opts...)
	if len(paths) == 0 {
		return nil, false
	}

	// take the longest path
	slices.SortFunc(paths, func(i, j []ast.Node) int {
		return -cmp.Compare(len(i), len(j))
	})
	return paths[0], true
}

func findDefinition(desc protoreflect.Descriptor, files linker.Files) (ast.NodeReference, error) {
	parentFile := desc.ParentFile()
	if parentFile == nil {
		return ast.NodeReference{}, fmt.Errorf("no parent file found for descriptor")
	}
	linkRes, ok := files.FindFileByPath(parentFile.Path()).(linker.Result)
	if !ok {
		return ast.NodeReference{}, fmt.Errorf("failed to find containing file for %q", parentFile.Path())
	}

	var node ast.Node
	switch desc := desc.(type) {
	case protoreflect.MessageDescriptor:
		node = linkRes.MessageNode(desc.(protoutil.DescriptorProtoWrapper).AsProto().(*descriptorpb.DescriptorProto)).MessageName()
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
	if _, ok := node.(ast.NoSourceNode); ok {
		return ast.NodeReference{}, fmt.Errorf("no source available")
	}
	return ast.NewNodeReference(linkRes.AST(), node), nil
}

func findNodeReferences(desc protoreflect.Descriptor, files linker.Files) <-chan ast.NodeReference {
	var wg sync.WaitGroup
	refs := make(chan ast.NodeReference, len(files))
	seen := sync.Map{}
	wg.Add(len(files))
	for _, res := range files {
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
