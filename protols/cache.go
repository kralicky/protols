package main

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sync/atomic"

	"github.com/bufbuild/protocompile"
	"github.com/bufbuild/protocompile/ast"
	"github.com/bufbuild/protocompile/linker"
	"github.com/bufbuild/protocompile/reporter"
	"github.com/kralicky/ragu"
	"github.com/samber/lo"
	"go.lsp.dev/protocol"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
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
	file, err := s.FindFileByPath(doc.URI.Filename())
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
	ast.Walk(fn, &ast.SimpleVisitor{
		DoVisitImportNode: func(node *ast.ImportNode) error {
			info := fn.NodeInfo(node.Name)
			start := info.Start()
			end := info.End()
			tokens = append(tokens, semanticToken{
				line:      uint32(start.Line - 1),
				start:     uint32(start.Col - 1),
				len:       uint32(end.Col - start.Col),
				tokenType: semanticTypeNamespace,
			})
			return nil
		},
		DoVisitMessageNode: func(node *ast.MessageNode) error {
			info := fn.NodeInfo(node.Name)
			start := info.Start()
			end := info.End()
			tokens = append(tokens, semanticToken{
				line:      uint32(start.Line - 1),
				start:     uint32(start.Col - 1),
				len:       uint32(end.Col - start.Col),
				tokenType: semanticTypeType,
			})
			return nil
		},
	})
	tokenData := make([]uint32, 0, len(tokens)*5)
	for _, token := range tokens {
		tokenData = append(tokenData, token.line, token.start, token.len, uint32(token.tokenType), uint32(token.mods))
	}
	return tokenData, nil
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
	f, err := s.FindFileByPath(file.URI.Filename())
	if err != nil {
		return nil, err
	}

	var symbols []any

	s.lg.Debug("computing document symbols", zap.String("file", file.URI.Filename()))
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
				Kind:           protocol.SymbolKindInterface,
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
						Kind:           protocol.SymbolKindMethod,
						Range:          toRange(fn.NodeInfo(node)),
						SelectionRange: toRange(fn.NodeInfo(node.Name)),
					}

					ast.Walk(node, &ast.SimpleVisitor{
						DoVisitRPCTypeNode: func(node *ast.RPCTypeNode) error {
							s.lg.Debug("found rpc type", zap.String("name", string(node.MessageType.AsIdentifier())), zap.String("service", string(node.MessageType.AsIdentifier())))
							rpcType := protocol.DocumentSymbol{
								Name:           string(node.MessageType.AsIdentifier()),
								Kind:           protocol.SymbolKindClass,
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
				Kind:           protocol.SymbolKindClass,
				Range:          toRange(fn.NodeInfo(node)),
				SelectionRange: toRange(fn.NodeInfo(node.Name)),
			}
			ast.Walk(node, &ast.SimpleVisitor{
				DoVisitFieldNode: func(node *ast.FieldNode) error {
					sym.Children = append(sym.Children, protocol.DocumentSymbol{
						Name:           string(node.Name.AsIdentifier()),
						Detail:         string(node.FldType.AsIdentifier()),
						Kind:           protocol.SymbolKindField,
						Range:          toRange(fn.NodeInfo(node)),
						SelectionRange: toRange(fn.NodeInfo(node.Name)),
					})
					return nil
				},
				DoVisitMapFieldNode: func(node *ast.MapFieldNode) error {
					sym.Children = append(sym.Children, protocol.DocumentSymbol{
						Name:           string(node.Name.AsIdentifier()),
						Detail:         fmt.Sprintf("map<%s, %s>", string(node.KeyField().Ident.AsIdentifier()), string(node.ValueField().Ident.AsIdentifier())),
						Kind:           protocol.SymbolKindField,
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
				Kind:           protocol.SymbolKindEnum,
				Range:          toRange(fn.NodeInfo(node)),
				SelectionRange: toRange(fn.NodeInfo(node.Name)),
			}
			ast.Walk(node, &ast.SimpleVisitor{
				DoVisitEnumValueNode: func(node *ast.EnumValueNode) error {
					sym.Children = append(sym.Children, protocol.DocumentSymbol{
						Name:           string(node.Name.AsIdentifier()),
						Kind:           protocol.SymbolKindEnumMember,
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
				Kind:           protocol.SymbolKindClass,
				Range:          toRange(fn.NodeInfo(node)),
				SelectionRange: toRange(fn.NodeInfo(node.Extendee)),
			}
			ast.Walk(node, &ast.SimpleVisitor{
				DoVisitFieldNode: func(node *ast.FieldNode) error {
					sym.Children = append(sym.Children, protocol.DocumentSymbol{
						Name:           string(node.Name.AsIdentifier()),
						Kind:           protocol.SymbolKindField,
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
