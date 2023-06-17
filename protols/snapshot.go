//go:build ignore

package main

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/bufbuild/protocompile/ast"
	"github.com/bufbuild/protocompile/linker"
	"golang.org/x/tools/gopls/pkg/lsp/protocol"
	"golang.org/x/tools/gopls/pkg/span"

	"go.uber.org/zap"
)

func (s *Snapshot) filePathToGoPackage(path string) string {
	dir, name := filepath.Split(path)
	dir = filepath.Clean(dir)
	pkg, ok := s.indexedGoPkgsByDir[dir]
	if !ok {
		s.lg.Debug("no go package found for directory", zap.String("dir", dir))
		return path
	}
	return filepath.Join(pkg, name)
}

func (s *Snapshot) blockingCompile(protos ...string) {
	s.resultsMu.Lock()
	defer s.resultsMu.Unlock()
	s.lg.Debug("compiling")
	res, err := s.compiler.Compile(context.TODO(), protos...)
	if err != nil {
		s.lg.With(
			zap.Error(err),
		).Debug("compiler error occurred")
	}
	if res != nil {
		// filter out nil results
		filtered := make(linker.Files, 0, len(res))
		for _, r := range res {
			if r != nil {
				filtered = append(filtered, r)
			}
		}
		s.results = filtered

		for _, r := range s.results {
			s.semanticDocuments[r.Path()] = NewSemanticDocument(r.(linker.Result).AST())
		}
	}
	s.lg.Debug("compiling done")
}
func (s *Snapshot) backgroundCompile(protos ...string) {
	go s.blockingCompile(protos...)
}

func (s *Snapshot) OnFileOpened(doc protocol.TextDocumentItem) {
	s.compiler.overlay.Create(span.URIFromURI(string(doc.URI)), []byte(doc.Text))
}

func (s *Snapshot) OnFileClosed(doc protocol.TextDocumentIdentifier) {
	s.compiler.overlay.Delete(span.URIFromURI(string(doc.URI)))
}

func (s *Snapshot) OnFileModified(f protocol.VersionedTextDocumentIdentifier, contentChanges []protocol.TextDocumentContentChangeEvent) error {
	s.lg.With(
		zap.String("file", string(f.URI)),
	).Debug("file modified")
	file, err := s.FindResultByPath(f.URI.SpanURI().Filename())
	if err != nil {
		return err
	}
	s.compiler.overlay.Update(span.URIFromPath(file.Path()), contentChanges)
	s.blockingCompile(file.Path())
	// file, err = s.FindResultByPath(u.Filename())
	if err != nil {
		return err
	}
	// s.lg.With(
	// 	zap.String("file", string(f.URI)),
	// 	zap.Int("changes", len(contentChanges)),
	// ).Debug("computing semantic tokens")
	return nil
	// return s.updateSemanticTokens(file, contentChanges)
}

func (s *Snapshot) OnFileDeleted(f protocol.FileDelete) error {
	return nil // TODO
}

func (s *Snapshot) OnFileCreated(f protocol.FileCreate) error {
	return nil // TODO
}

func (s *Snapshot) OnFileSaved(f *protocol.DidSaveTextDocumentParams) error {
	s.lg.With(
		zap.String("file", string(f.TextDocument.URI)),
	).Debug("file modified")

	file, err := s.FindResultByPath(f.TextDocument.URI.SpanURI().Filename())
	if err != nil {
		return err
	}
	s.blockingCompile(file.Path())
	// file, err = s.FindResultByPath(u.Filename())
	// if err != nil {
	// 	return err
	// }
	// s.lg.With(
	// 	zap.String("file", string(f.TextDocument.URI)),
	// ).Debug("computing semantic tokens")
	return nil
	// return s.updateSemanticTokens(file, nil)
}

// func (s *Snapshot) FindFileByPath(path string) (linker.Result, error) {
// 	s.resultsMu.RLock()
// 	defer s.resultsMu.RUnlock()
// 	f := s.results.FindFileByPath(path)
// 	if f == nil {
// 		return nil, fmt.Errorf("file not found: %s", path)
// 	}
// 	return f.(linker.Result), nil
// }

// func (s *Snapshot) FindExtensionByName(field protoreflect.FullName) (protoreflect.ExtensionType, error) {
// 	s.resultsMu.RLock()
// 	defer s.resultsMu.RUnlock()
// 	return s.results.AsResolver().FindExtensionByName(field)
// }

func (s *Snapshot) ComputeSemanticTokens(doc protocol.TextDocumentIdentifier) ([]uint32, error) {
	file, err := s.FindResultByPath(doc.URI.SpanURI().Filename())
	if err != nil {
		return nil, err
	}
	return s.computeSemanticTokensForFile(file)
}

func (s *Snapshot) ComputeSemanticTokensRange(doc protocol.TextDocumentIdentifier, rng protocol.Range) ([]uint32, error) {
	file, err := s.FindResultByPath(doc.URI.SpanURI().Filename())
	if err != nil {
		return nil, err
	}
	return s.computeSemanticTokensForRange(file, rng)
}

func (s *Snapshot) computeSemanticTokensForFile(file linker.Result) ([]uint32, error) {
	sd := s.semanticDocuments[file.Path()]
	info := sd.fileNode.NodeInfo(sd.fileNode)
	rng := positionsToRange(info.Start(), info.End())
	result, err := s.computeSemanticTokensForRange(file, rng)
	if err != nil {
		return nil, err
	}
	s.lg.With(
		zap.String("file", file.Path()),
		zap.Any("range", rng),
		zap.Int("tokens", len(result)),
	).Debug("querying semantic tokens for file")
	return result, nil
}

func (s *Snapshot) computeSemanticTokensForRange(file linker.Result, rng protocol.Range) ([]uint32, error) {
	sd := s.semanticDocuments[file.Path()]
	tokens, _ := sd.semanticTokens.AllIntersections(rangeToSourcePositions(rng))
	result := encodeSemanticTokens(tokens)
	s.lg.With(
		zap.String("file", file.Path()),
		zap.Any("range", rng),
		zap.Int("tokens", len(result)),
	).Debug("querying semantic tokens for range")
	return result, nil
}

func (s *Snapshot) updateSemanticTokens(file linker.Result, contentChanges []protocol.TextDocumentContentChangeEvent) error {
	sd := s.semanticDocuments[file.Path()]
	if len(contentChanges) == 0 {
		sd.buildTokens(sd.fileNode)
		return nil
	}

	for _, change := range contentChanges {
		rng := change.Range
		parent, ok := sd.semanticTokens.Ceil(rangeToSourcePositions(*rng))
		if !ok {
			s.lg.Error("no semantic tokens for range", zap.String("file", file.Path()), zap.Any("range", rng))
			return fmt.Errorf("no semantic tokens for range")
		}
		s.lg.With(
			zap.String("file", file.Path()),
			zap.Any("range", rng),
			zap.Any("parent", parent),
		).Debug("updating semantic tokens for range")
		info := sd.fileNode.NodeInfo(parent.node)
		if err := sd.semanticTokens.Delete(info.Start(), info.End()); err != nil {
			return err
		}
		sd.buildTokens(parent.node)
	}
	return nil
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

func rangeToSourcePositions(rng protocol.Range) (start, end ast.SourcePos) {
	start = ast.SourcePos{
		Line: int(rng.Start.Line) + 1,
		Col:  int(rng.Start.Character) + 1,
	}
	end = ast.SourcePos{
		Line: int(rng.End.Line) + 1,
		Col:  int(rng.End.Character) + 1,
	}
	return
}

func (s *Snapshot) DocumentSymbolsForFile(file protocol.TextDocumentIdentifier) ([]any, error) {
	f, err := s.FindResultByPath(file.URI.SpanURI().Filename())
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
					// s.lg.Debug("found rpc", zap.String("name", string(node.Name.AsIdentifier())), zap.String("service", string(node.Name.AsIdentifier())))
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
							// s.lg.Debug("found rpc type", zap.String("name", string(node.MessageType.AsIdentifier())), zap.String("service", string(node.MessageType.AsIdentifier())))
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
