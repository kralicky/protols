package main

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"sync/atomic"

	"github.com/bufbuild/protocompile"
	"github.com/bufbuild/protocompile/ast"
	"github.com/bufbuild/protocompile/linker"
	"github.com/bufbuild/protocompile/reporter"
	"github.com/kralicky/ragu"
	protocol "github.com/kralicky/ragu/cmd/protols/protocol"
	"github.com/samber/lo"
	"go.lsp.dev/uri"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
	"google.golang.org/protobuf/types/descriptorpb"
)

// Cache is responsible for keeping track of all the known proto source files
// and definitions.
type Cache struct {
	lg      *zap.Logger
	sources []string

	sf   singleflight.Group
	snap atomic.Pointer[Snapshot]
}

type Snapshot struct {
	lg              *zap.Logger
	Files           linker.Files
	SourcePackages  map[string]string
	SourcePkgDirs   map[string]string
	SourceFilenames map[string]string
}

// NewCache creates a new cache.
func NewCache(sources []string, lg *zap.Logger) *Cache {
	return &Cache{
		lg:      lg,
		sources: sources,
	}
}

func (c *Cache) Reindex(ctx context.Context) error {
	c.sf.Do("reindex", func() (interface{}, error) {
		snap := &Snapshot{
			lg:              c.lg,
			SourcePackages:  map[string]string{},
			SourcePkgDirs:   map[string]string{},
			SourceFilenames: map[string]string{},
		}

		resolved, err := ragu.ResolvePatterns(c.sources)
		if err != nil {
			return nil, err
		}
		c.lg.With(
			zap.Strings("sources", c.sources),
			zap.Strings("files", resolved),
		).Debug("sources resolved")

		for _, source := range resolved {
			goPkg, err := ragu.FastLookupGoModule(source)
			if err != nil {
				c.lg.With(
					zap.String("source", source),
					zap.Error(err),
				).Debug("failed to lookup go module")
				continue
			}
			snap.SourcePkgDirs[goPkg] = filepath.Dir(source)
			snap.SourcePackages[path.Join(goPkg, path.Base(source))] = source
			snap.SourceFilenames[source] = path.Join(goPkg, path.Base(source))
		}
		accessor := ragu.SourceAccessor(snap.SourcePackages)
		res := protocompile.WithStandardImports(ragu.NewResolver(accessor))
		compiler := protocompile.Compiler{
			Resolver:       res,
			MaxParallelism: -1,
			SourceInfoMode: protocompile.SourceInfoExtraComments | protocompile.SourceInfoExtraOptionLocations,
			Reporter:       reporter.NewReporter(nil, nil),
			RetainASTs:     true,
		}
		snap.Files, err = compiler.Compile(ctx, lo.Keys(snap.SourcePackages)...)
		if err != nil {
			c.lg.With(
				zap.Strings("sources", c.sources),
				zap.Error(err),
			).Debug("failed to compile")
			return nil, err
		}
		c.snap.Store(snap)
		return nil, nil
	})
	return nil
}

func (c *Cache) Snapshot() *Snapshot {
	return c.snap.Load()
}

func (s *Snapshot) FindFileByPath(path string) (linker.Result, error) {
	if f, ok := s.SourceFilenames[path]; ok {
		path = f
	}
	f := s.Files.FindFileByPath(path)
	if f == nil {
		return nil, os.ErrNotExist
	}
	return f.(linker.Result), nil
}

func (s *Snapshot) ComputeSemanticTokens(doc protocol.TextDocumentIdentifier) ([]uint32, error) {
	u, err := uri.Parse(string(doc.URI))
	if err != nil {
		return nil, err
	}
	file, err := s.FindFileByPath(u.Filename())
	if err != nil {
		return nil, err
	}
	return s.computeSemanticTokensForFile(file)
}

type tokenType uint32

const (
	semanticTypeNamespace tokenType = iota
	semanticTypeType
	semanticTypeClass
	semanticTypeEnum
	semanticTypeInterface
	semanticTypeStruct
	semanticTypeTypeParameter
	semanticTypeParameter
	semanticTypeVariable
	semanticTypeProperty
	semanticTypeEnumMember
	semanticTypeEvent
	semanticTypeFunction
	semanticTypeMethod
	semanticTypeMacro
	semanticTypeKeyword
	semanticTypeModifier
	semanticTypeComment
	semanticTypeString
	semanticTypeNumber
	semanticTypeRegexp
	semanticTypeOperator
)

type tokenModifier uint32

const (
	semanticModifierDeclaration tokenModifier = 1 << iota
	semanticModifierDefinition
	semanticModifierReadonly
	semanticModifierStatic
	semanticModifierDeprecated
	semanticModifierAbstract
	semanticModifierAsync
	semanticModifierModification
	semanticModifierDocumentation
	semanticModifierDefaultLibrary
)

type semanticToken struct {
	line, start uint32
	len         uint32
	tokenType   tokenType
	mods        tokenModifier
}

func (s *Snapshot) computeSemanticTokensForFile(file linker.Result) ([]uint32, error) {
	var tokens []semanticToken

	fn := file.AST()

	mktokens := func(node ast.Node, tt tokenType, mods ...tokenModifier) []semanticToken {
		info := fn.NodeInfo(node)
		rng := toRange(info)
		if len(mods) == 0 {
			mods = append(mods, 0)
		}
		tks := []semanticToken{
			{
				line:      rng.Start.Line,
				start:     rng.Start.Character,
				len:       rng.End.Character - rng.Start.Character,
				tokenType: tt,
				mods:      mods[0],
			},
		}
		for i := 0; i < info.LeadingComments().Len(); i++ {
			comment := info.LeadingComments().Index(i)
			rng := toRange(comment)
			tks = append(tks, semanticToken{
				line:  rng.Start.Line,
				start: rng.Start.Character,
				// no idea why but comments need an extra character lol
				len:       rng.End.Character - rng.Start.Character + 1,
				tokenType: semanticTypeComment,
			})
		}
		return tks
	}

	var tracker ast.AncestorTracker
	ast.Walk(fn, &ast.SimpleVisitor{
		DoVisitStringLiteralNode: func(node *ast.StringLiteralNode) error {
			tokens = append(tokens, mktokens(node, semanticTypeString)...)
			return nil
		},
		DoVisitUintLiteralNode: func(node *ast.UintLiteralNode) error {
			tokens = append(tokens, mktokens(node, semanticTypeNumber)...)
			return nil
		},
		DoVisitFloatLiteralNode: func(node *ast.FloatLiteralNode) error {
			tokens = append(tokens, mktokens(node, semanticTypeNumber)...)
			return nil
		},
		DoVisitSpecialFloatLiteralNode: func(node *ast.SpecialFloatLiteralNode) error {
			tokens = append(tokens, mktokens(node, semanticTypeNumber)...)
			return nil
		},
		DoVisitKeywordNode: func(node *ast.KeywordNode) error {
			tokens = append(tokens, mktokens(node, semanticTypeKeyword)...)
			return nil
		},
		DoVisitRuneNode: func(node *ast.RuneNode) error {
			tks := mktokens(node, semanticTypeOperator)
			switch node.Rune {
			case '{', '}', ';', '.':
				tks = tks[1:] // skip some tokens we don't care to highlight
			}
			tokens = append(tokens, tks...)
			return nil
		},
		DoVisitIdentNode: func(node *ast.IdentNode) error {
			// first check for primitive types
			switch node.Val {
			case "double", "float",
				"int32", "int64",
				"uint32", "uint64",
				"sint32", "sint64",
				"fixed32", "fixed64",
				"sfixed32", "sfixed64",
				"bool", "string", "bytes":
				tokens = append(tokens, mktokens(node, semanticTypeType, semanticModifierDefaultLibrary)...)
				return nil
			}

			path := tracker.Path()
			switch parentNode := path[len(path)-2].(type) {
			case *ast.MessageNode:
				s.lg.Debug("ident > message", zap.String("ident", node.Val), zap.String("message", parentNode.Name.Val))
				tokens = append(tokens, mktokens(node, semanticTypeClass)...)
			case *ast.FieldNode:
				s.lg.Debug("ident > field", zap.String("ident", node.Val), zap.String("field", parentNode.Name.Val))
				// need to check the descriptors to disambiguate field nodes.
				// from here we need to use the descriptorpb types.
				desc := file.Descriptor(parentNode)
				if desc == nil {
					s.lg.Debug("no descriptor for field", zap.String("field", parentNode.Name.Val))
					break
				}
				switch desc.(type) {
				case *descriptorpb.FieldDescriptorProto:
					s.lg.Debug("ident > field > field descriptor", zap.String("field", parentNode.Name.Val))
					// one of several tokens within a field
					if node.Val == parentNode.Name.Val {
						// field name
						s.lg.Debug("ident > field > field descriptor > name", zap.String("field", parentNode.Name.Val))
						tokens = append(tokens, mktokens(node, semanticTypeProperty)...)
					} else {
						// field type?
						s.lg.Debug("ident > field > field descriptor > type", zap.String("field", parentNode.Name.Val))
						tokens = append(tokens, mktokens(node, semanticTypeType)...)
					}

				case *descriptorpb.DescriptorProto:
					s.lg.Debug("ident > field > message descriptor", zap.String("field", parentNode.Name.Val))
					tokens = append(tokens, mktokens(node, semanticTypeType)...)
				case *descriptorpb.EnumDescriptorProto:
					s.lg.Debug("ident > field > enum descriptor", zap.String("field", parentNode.Name.Val))
					tokens = append(tokens, mktokens(node, semanticTypeEnum)...)
				default:
					s.lg.Debug("unknown descriptor type", zap.String("type", fmt.Sprintf("%T", desc)))
				}
			case *ast.RPCTypeNode:
				s.lg.Debug("ident > rpc type", zap.String("ident", node.Val))
				tokens = append(tokens, mktokens(node, semanticTypeType)...)
			case *ast.RPCNode:
				s.lg.Debug("ident > rpc", zap.String("ident", node.Val))
				tokens = append(tokens, mktokens(node, semanticTypeMethod)...)
			case *ast.ServiceNode:
				s.lg.Debug("ident > service", zap.String("ident", node.Val))
				tokens = append(tokens, mktokens(node, semanticTypeInterface, semanticModifierDeclaration)...)
			case *ast.CompoundIdentNode:
				switch path[len(path)-3].(type) {
				case *ast.FieldNode, *ast.RPCTypeNode:
					// one of several tokens within a field
					if node.Val == parentNode.Components[len(parentNode.Components)-1].Val {
						// last component of a compound ident, which is the type
						tokens = append(tokens, mktokens(node, semanticTypeType)...)
					} else {
						// not the last component, package qualifier
						tokens = append(tokens, mktokens(node, semanticTypeNamespace)...)
					}
				case *ast.FieldReferenceNode:
					tokens = append(tokens, mktokens(node, semanticTypeProperty)...)
				// case *ast.MapTypeNode:
				// 	switch node.Val {
				// 	case grandparentNode.KeyType.Val:
				// 		// key type
				// 		tokens = append(tokens, mktokens(node, semanticTypeType)...)
				// 	case string(grandparentNode.ValueType.AsIdentifier()):
				// 		// value type
				// 		if node.Val == parentNode.Components[len(parentNode.Components)-1].Val {
				// 			// last component of a compound ident, which is the type
				// 			tokens = append(tokens, mktokens(node, semanticTypeType)...)
				// 		} else {
				// 			// not the last component, package qualifier
				// 			tokens = append(tokens, mktokens(node, semanticTypeProperty)...)
				// 		}
				// 	}
				default:
					s.lg.Warn("unknown compound ident ancestor", zap.String("type", fmt.Sprintf("%T", path[len(path)-3])))
				}
			// case *ast.MapFieldNode:
			// 	switch grandparentNode := path[len(path)-3].(type) {
			// 	case *ast.MapTypeNode:
			// 		switch node.Val {
			// 		case grandparentNode.KeyType.Val:
			// 			// key type
			// 			tokens = append(tokens, mktokens(node, semanticTypeType)...)
			// 		case string(grandparentNode.ValueType.AsIdentifier()):
			// 			// value type
			// 			tokens = append(tokens, mktokens(node, semanticTypeType)...)
			// 		default:
			// 			// field name
			// 			tokens = append(tokens, mktokens(node, semanticTypeProperty)...)
			// 		}
			// 	case *ast.IdentNode:
			// 		switch node.Val {
			// 		case parentNode.Name.Val:
			// 			// field name
			// 			tokens = append(tokens, mktokens(node, semanticTypeProperty)...)
			// 		case parentNode.MapType.KeyType.Val:
			// 			// key type
			// 			tokens = append(tokens, mktokens(node, semanticTypeType)...)
			// 		case string(parentNode.MapType.ValueType.AsIdentifier()):
			// 			// value type
			// 			tokens = append(tokens, mktokens(node, semanticTypeType)...)
			// 		}
			// 	case *ast.CompoundIdentNode:
			// 		switch node.Val {
			// 		case string(parentNode.MapType.ValueType.AsIdentifier()):
			// 			// compound map value type
			// 			// if the last component of the compound ident is the value type, highlight it
			// 			if node.Val == grandparentNode.Components[len(grandparentNode.Components)-1].Val {
			// 				tokens = append(tokens, mktokens(node, semanticTypeType)...)
			// 			} else {
			// 				tokens = append(tokens, mktokens(node, semanticTypeNamespace)...)
			// 			}
			// 		}
			// 	case *ast.MessageNode:
			// 		grandparentNode.MessageBody.Decls
			// 		s.lg.Debug("map field > message", zap.String("field", parentNode.Name.Val))
			// 	default:
			// 		s.lg.With(
			// 			zap.String("node", fmt.Sprintf("%T", node)),
			// 			zap.String("parent", fmt.Sprintf("%T", parentNode)),
			// 			zap.String("grandparent", fmt.Sprintf("%T", grandparentNode)),
			// 		).Warn("unknown map field ancestor")
			// 	}
			case *ast.PackageNode:
				if node.Val == string(parentNode.Name.AsIdentifier()) {
					tokens = append(tokens, mktokens(node, semanticTypeNamespace)...)
				}
			case *ast.FieldReferenceNode:
				tokens = append(tokens, mktokens(node, semanticTypeProperty)...)
			default:
				s.lg.Warn("unknown ident ancestor", zap.String("type", fmt.Sprintf("%T", parentNode)))
			}

			return nil
		},
	}, tracker.AsWalkOptions()...)

	sort.Slice(tokens, func(i, j int) bool {
		if tokens[i].line != tokens[j].line {
			return tokens[i].line < tokens[j].line
		}
		return tokens[i].start < tokens[j].start
	})
	x := make([]uint32, len(tokens)*5)
	var j int
	var last semanticToken
	for i := 0; i < len(tokens); i++ {
		item := tokens[i]

		if j == 0 {
			x[0] = tokens[0].line
		} else {
			x[j] = item.line - last.line
		}
		x[j+1] = item.start
		if j > 0 && x[j] == 0 {
			x[j+1] = item.start - last.start
		}
		x[j+2] = item.len
		x[j+3] = uint32(item.tokenType)
		mask := uint32(item.mods)
		x[j+4] = uint32(mask)
		j += 5
		last = item
	}
	return x[:j], nil

}

type ranger interface {
	Start() ast.SourcePos
	End() ast.SourcePos
}

func toRange[T ranger](t T) protocol.Range {
	return positionsToRange(t.Start(), t.End())
}

func positionsToRange(start, end ast.SourcePos) protocol.Range {
	return protocol.Range{
		Start: protocol.Position{
			Line:      uint32(start.Line - 1),
			Character: uint32(start.Col - 1),
		},
		End: protocol.Position{
			Line:      uint32(end.Line - 1),
			Character: uint32(end.Col - 1),
		},
	}
}

func (s *Snapshot) DocumentSymbolsForFile(file protocol.TextDocumentIdentifier) ([]any, error) {
	u, err := uri.Parse(string(file.URI))
	if err != nil {
		return nil, err
	}
	f, err := s.FindFileByPath(u.Filename())
	if err != nil {
		return nil, err
	}

	var symbols []any

	s.lg.Debug("computing document symbols", zap.String("file", string(file.URI)))
	fn := f.AST()
	ast.Walk(fn, &ast.SimpleVisitor{
		// DoVisitImportNode: func(node *ast.ImportNode) error {
		// 	s.lg.Debug("found import", zap.String("name", string(node.Name.AsString())))
		// 	symbols = append(symbols, protocol.DocumentSymbol{
		// 		Name:           string(node.Name.AsString()),
		// 		Kind:           protocol.SymbolKindNamespace,
		// 		Range:          posToRange(fn.NodeInfo(node)),
		// 		SelectionRange: posToRange(fn.NodeInfo(node.Name)),
		// 	})
		// 	return nil
		// },
		DoVisitServiceNode: func(node *ast.ServiceNode) error {
			s.lg.Debug("found service", zap.String("name", string(node.Name.AsIdentifier())))
			service := protocol.DocumentSymbol{
				Name:           string(node.Name.AsIdentifier()),
				Kind:           protocol.Interface,
				Range:          toRange(fn.NodeInfo(node)),
				SelectionRange: toRange(fn.NodeInfo(node.Name)),
			}

			ast.Walk(node, &ast.SimpleVisitor{
				DoVisitRPCNode: func(node *ast.RPCNode) error {
					s.lg.Debug("found rpc", zap.String("name", string(node.Name.AsIdentifier())), zap.String("service", string(node.Name.AsIdentifier())))
					var detail string
					switch {
					case node.Input.Stream != nil && node.Output.Stream != nil:
						detail = "stream (bidirectional)"
					case node.Input.Stream != nil:
						detail = "stream (client)"
					case node.Output.Stream != nil:
						detail = "stream (server)"
					default:
						detail = "unary"
					}
					rpc := protocol.DocumentSymbol{
						Name:           string(node.Name.AsIdentifier()),
						Detail:         detail,
						Kind:           protocol.Method,
						Range:          toRange(fn.NodeInfo(node)),
						SelectionRange: toRange(fn.NodeInfo(node.Name)),
					}

					ast.Walk(node, &ast.SimpleVisitor{
						DoVisitRPCTypeNode: func(node *ast.RPCTypeNode) error {
							s.lg.Debug("found rpc type", zap.String("name", string(node.MessageType.AsIdentifier())), zap.String("service", string(node.MessageType.AsIdentifier())))
							rpcType := protocol.DocumentSymbol{
								Name:           string(node.MessageType.AsIdentifier()),
								Kind:           protocol.Class,
								Range:          toRange(fn.NodeInfo(node)),
								SelectionRange: toRange(fn.NodeInfo(node.MessageType)),
							}
							rpc.Children = append(rpc.Children, rpcType)
							return nil
						},
					})
					service.Children = append(service.Children, rpc)
					return nil
				},
			})
			symbols = append(symbols, service)
			return nil
		},
		DoVisitMessageNode: func(node *ast.MessageNode) error {
			sym := protocol.DocumentSymbol{
				Name:           string(node.Name.AsIdentifier()),
				Kind:           protocol.Class,
				Range:          toRange(fn.NodeInfo(node)),
				SelectionRange: toRange(fn.NodeInfo(node.Name)),
			}
			ast.Walk(node, &ast.SimpleVisitor{
				DoVisitFieldNode: func(node *ast.FieldNode) error {
					sym.Children = append(sym.Children, protocol.DocumentSymbol{
						Name:           string(node.Name.AsIdentifier()),
						Detail:         string(node.FldType.AsIdentifier()),
						Kind:           protocol.Field,
						Range:          toRange(fn.NodeInfo(node)),
						SelectionRange: toRange(fn.NodeInfo(node.Name)),
					})
					return nil
				},
				DoVisitMapFieldNode: func(node *ast.MapFieldNode) error {
					sym.Children = append(sym.Children, protocol.DocumentSymbol{
						Name:           string(node.Name.AsIdentifier()),
						Detail:         fmt.Sprintf("map<%s, %s>", string(node.KeyField().Ident.AsIdentifier()), string(node.ValueField().Ident.AsIdentifier())),
						Kind:           protocol.Field,
						Range:          toRange(fn.NodeInfo(node)),
						SelectionRange: toRange(fn.NodeInfo(node.Name)),
					})
					return nil
				},
			})
			symbols = append(symbols, sym)
			return nil
		},
		DoVisitEnumNode: func(node *ast.EnumNode) error {
			sym := protocol.DocumentSymbol{
				Name:           string(node.Name.AsIdentifier()),
				Kind:           protocol.Enum,
				Range:          toRange(fn.NodeInfo(node)),
				SelectionRange: toRange(fn.NodeInfo(node.Name)),
			}
			ast.Walk(node, &ast.SimpleVisitor{
				DoVisitEnumValueNode: func(node *ast.EnumValueNode) error {
					sym.Children = append(sym.Children, protocol.DocumentSymbol{
						Name:           string(node.Name.AsIdentifier()),
						Kind:           protocol.EnumMember,
						Range:          toRange(fn.NodeInfo(node)),
						SelectionRange: toRange(fn.NodeInfo(node.Name)),
					})
					return nil
				},
			})
			symbols = append(symbols, sym)
			return nil
		},
		DoVisitExtendNode: func(node *ast.ExtendNode) error {
			sym := protocol.DocumentSymbol{
				Name:           string(node.Extendee.AsIdentifier()),
				Kind:           protocol.Class,
				Range:          toRange(fn.NodeInfo(node)),
				SelectionRange: toRange(fn.NodeInfo(node.Extendee)),
			}
			ast.Walk(node, &ast.SimpleVisitor{
				DoVisitFieldNode: func(node *ast.FieldNode) error {
					sym.Children = append(sym.Children, protocol.DocumentSymbol{
						Name:           string(node.Name.AsIdentifier()),
						Kind:           protocol.Field,
						Range:          toRange(fn.NodeInfo(node)),
						SelectionRange: toRange(fn.NodeInfo(node.Name)),
					})
					return nil
				},
			})
			symbols = append(symbols, sym)
			return nil
		},
	})
	return symbols, nil
}
