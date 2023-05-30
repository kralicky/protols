package main

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/bufbuild/protocompile/ast"
	"github.com/bufbuild/protocompile/linker"
	"github.com/bufbuild/protocompile/protoutil"
	"github.com/jhump/protoreflect/desc"
	"github.com/jhump/protoreflect/desc/protoprint"
	"google.golang.org/protobuf/reflect/protoreflect"

	"go.lsp.dev/jsonrpc2"
	protocol "go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"go.uber.org/zap"
)

type Server struct {
	lg *zap.Logger
	c  *Cache

	openedDocumentsMu sync.Mutex
	openedDocuments   map[protocol.DocumentURI]linker.Result
}

func NewServer(lg *zap.Logger) *Server {
	return &Server{
		lg:              lg,
		openedDocuments: map[protocol.DocumentURI]linker.Result{},
	}
}

// Initialize implements protocol.Server.
func (s *Server) Initialize(ctx context.Context, params *protocol.InitializeParams) (result *protocol.InitializeResult, err error) {
	sources := []string{}
	for _, ws := range params.WorkspaceFolders {
		wsUri := ws.URI
		u, _ := uri.Parse(wsUri)
		sources = append(sources, u.Filename()+"/**/*.proto")
	}
	s.c = NewCache(sources, s.lg.Named("cache"))
	if err := s.c.Reindex(ctx); err != nil {
		return nil, fmt.Errorf("indexing failed: %w", err)
	}

	s.lg.Debug("Initialize", zap.Strings("sources", sources), zap.Any("initOpts", params.InitializationOptions))
	return &protocol.InitializeResult{
		Capabilities: protocol.ServerCapabilities{
			TextDocumentSync:       protocol.TextDocumentSyncKindFull,
			HoverProvider:          true,
			DeclarationProvider:    true,
			TypeDefinitionProvider: true,
			// ReferencesProvider: true,
			// WorkspaceSymbolProvider: true,
			CompletionProvider: &protocol.CompletionOptions{
				TriggerCharacters: []string{"."},
			},
			DefinitionProvider: true,
			// DocumentSymbolProvider: true,
		},

		ServerInfo: &protocol.ServerInfo{
			Name:    "protols",
			Version: "0.0.1",
		},
	}, nil

}

// DidOpen implements protocol.Server.
func (s *Server) DidOpen(ctx context.Context, params *protocol.DidOpenTextDocumentParams) (err error) {
	s.lg.Debug("DidOpen", zap.String("uri", params.TextDocument.URI.Filename()))

	res, err := s.c.FindFileByPath(params.TextDocument.URI.Filename())
	if err != nil {
		return fmt.Errorf("could not find file %q: %w", params.TextDocument.URI.Filename(), err)
	}

	s.openedDocumentsMu.Lock()
	s.openedDocuments[params.TextDocument.URI] = res
	s.openedDocumentsMu.Unlock()

	return nil
}

// DidClose implements protocol.Server.
func (s *Server) DidClose(ctx context.Context, params *protocol.DidCloseTextDocumentParams) (err error) {
	s.lg.Debug("DidClose", zap.String("uri", params.TextDocument.URI.Filename()))
	s.openedDocumentsMu.Lock()
	delete(s.openedDocuments, params.TextDocument.URI)
	s.openedDocumentsMu.Unlock()

	return nil
}

// Completion implements protocol.Server.
func (s *Server) Completion(ctx context.Context, params *protocol.CompletionParams) (result *protocol.CompletionList, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// Initialized implements protocol.Server.
func (s *Server) Initialized(ctx context.Context, params *protocol.InitializedParams) (err error) {
	s.lg.Debug("Initialized")
	return nil
}

// CodeAction implements protocol.Server.
func (s *Server) CodeAction(ctx context.Context, params *protocol.CodeActionParams) (result []protocol.CodeAction, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// CodeLens implements protocol.Server.
func (s *Server) CodeLens(ctx context.Context, params *protocol.CodeLensParams) (result []protocol.CodeLens, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// CodeLensRefresh implements protocol.Server.
func (s *Server) CodeLensRefresh(ctx context.Context) (err error) {
	return jsonrpc2.ErrMethodNotFound
}

// CodeLensResolve implements protocol.Server.
func (s *Server) CodeLensResolve(ctx context.Context, params *protocol.CodeLens) (result *protocol.CodeLens, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// ColorPresentation implements protocol.Server.
func (s *Server) ColorPresentation(ctx context.Context, params *protocol.ColorPresentationParams) (result []protocol.ColorPresentation, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// CompletionResolve implements protocol.Server.
func (s *Server) CompletionResolve(ctx context.Context, params *protocol.CompletionItem) (result *protocol.CompletionItem, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// Declaration implements protocol.Server.
func (s *Server) Declaration(ctx context.Context, params *protocol.DeclarationParams) (result []protocol.Location, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// Definition implements protocol.Server.
func (s *Server) Definition(ctx context.Context, params *protocol.DefinitionParams) (result []protocol.Location, err error) {
	// return nil, jsonrpc2.ErrMethodNotFound
	s.lg.Debug("Definition Request", zap.String("uri", params.TextDocument.URI.Filename()), zap.Int("line", int(params.Position.Line)), zap.Int("col", int(params.Position.Character)))
	desc, err := findRelevantDescriptorAtLocation(&params.TextDocumentPositionParams, s.c, s.lg)
	if err != nil {
		return nil, err
	}
	parentFile := desc.ParentFile()
	if parentFile == nil {
		return nil, errors.New("no parent file found for descriptor")
	}
	containingFileResolver, err := s.c.FindFileByPath(parentFile.Path())
	if err != nil {
		return nil, fmt.Errorf("failed to find containing file for %q: %w", parentFile.Path(), err)
	}
	var node ast.Node
	switch desc := desc.(type) {
	case protoreflect.MessageDescriptor:
		node = containingFileResolver.MessageNode(protoutil.ProtoFromMessageDescriptor(desc))
	case protoreflect.EnumDescriptor:
		node = containingFileResolver.EnumNode(protoutil.ProtoFromEnumDescriptor(desc))
	case protoreflect.ServiceDescriptor:
		node = containingFileResolver.ServiceNode(protoutil.ProtoFromServiceDescriptor(desc))
	case protoreflect.MethodDescriptor:
		node = containingFileResolver.MethodNode(protoutil.ProtoFromMethodDescriptor(desc))
	case protoreflect.FieldDescriptor:
		node = containingFileResolver.FieldNode(protoutil.ProtoFromFieldDescriptor(desc))
	case protoreflect.EnumValueDescriptor:
		node = containingFileResolver.EnumValueNode(protoutil.ProtoFromEnumValueDescriptor(desc))
	case protoreflect.OneofDescriptor:
		node = containingFileResolver.OneofNode(protoutil.ProtoFromOneofDescriptor(desc))
	case protoreflect.FileDescriptor:
		node = containingFileResolver.FileNode()
	default:
		return nil, fmt.Errorf("unexpected descriptor type %T", desc)
	}

	info := containingFileResolver.AST().NodeInfo(node)
	return []protocol.Location{
		{
			URI: uri.File(s.c.sourcePackages[containingFileResolver.Path()]),
			Range: protocol.Range{
				Start: protocol.Position{
					Line:      uint32(info.Start().Line - 1),
					Character: uint32(info.Start().Col - 1),
				},
				End: protocol.Position{
					Line:      uint32(info.End().Line - 1),
					Character: uint32(info.End().Col - 1),
				},
			},
		},
	}, nil
}

// Hover implements protocol.Server.
func (s *Server) Hover(ctx context.Context, params *protocol.HoverParams) (result *protocol.Hover, err error) {
	s.lg.Debug("Hover Request", zap.String("uri", params.TextDocument.URI.Filename()), zap.Int("line", int(params.Position.Line)), zap.Int("col", int(params.Position.Character)))
	d, err := findRelevantDescriptorAtLocation(&params.TextDocumentPositionParams, s.c, s.lg)
	if err != nil {
		return nil, err
	}
	wrap, err := desc.WrapDescriptor(d)
	if err != nil {
		return nil, err
	}
	printer := protoprint.Printer{
		SortElements: true,
		Indent:       "  ",
		Compact:      true,
	}
	str, err := printer.PrintProtoToString(wrap)
	if err != nil {
		return nil, err
	}
	return &protocol.Hover{
		Contents: protocol.MarkupContent{
			Kind:  protocol.PlainText,
			Value: str,
		},
	}, nil
}

// DidChange implements protocol.Server.
func (s *Server) DidChange(ctx context.Context, params *protocol.DidChangeTextDocumentParams) (err error) {
	return jsonrpc2.ErrMethodNotFound
}

// DidChangeConfiguration implements protocol.Server.
func (s *Server) DidChangeConfiguration(ctx context.Context, params *protocol.DidChangeConfigurationParams) (err error) {
	return jsonrpc2.ErrMethodNotFound
}

// DidChangeWatchedFiles implements protocol.Server.
func (s *Server) DidChangeWatchedFiles(ctx context.Context, params *protocol.DidChangeWatchedFilesParams) (err error) {
	return jsonrpc2.ErrMethodNotFound
}

// DidChangeWorkspaceFolders implements protocol.Server.
func (s *Server) DidChangeWorkspaceFolders(ctx context.Context, params *protocol.DidChangeWorkspaceFoldersParams) (err error) {
	return jsonrpc2.ErrMethodNotFound
}

// DidCreateFiles implements protocol.Server.
func (s *Server) DidCreateFiles(ctx context.Context, params *protocol.CreateFilesParams) (err error) {
	return jsonrpc2.ErrMethodNotFound
}

// DidDeleteFiles implements protocol.Server.
func (s *Server) DidDeleteFiles(ctx context.Context, params *protocol.DeleteFilesParams) (err error) {
	return jsonrpc2.ErrMethodNotFound
}

// DidRenameFiles implements protocol.Server.
func (s *Server) DidRenameFiles(ctx context.Context, params *protocol.RenameFilesParams) (err error) {
	return jsonrpc2.ErrMethodNotFound
}

// DidSave implements protocol.Server.
func (s *Server) DidSave(ctx context.Context, params *protocol.DidSaveTextDocumentParams) (err error) {
	return jsonrpc2.ErrMethodNotFound
}

// DocumentColor implements protocol.Server.
func (s *Server) DocumentColor(ctx context.Context, params *protocol.DocumentColorParams) (result []protocol.ColorInformation, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// DocumentHighlight implements protocol.Server.
func (s *Server) DocumentHighlight(ctx context.Context, params *protocol.DocumentHighlightParams) (result []protocol.DocumentHighlight, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// DocumentLink implements protocol.Server.
func (s *Server) DocumentLink(ctx context.Context, params *protocol.DocumentLinkParams) (result []protocol.DocumentLink, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// DocumentLinkResolve implements protocol.Server.
func (s *Server) DocumentLinkResolve(ctx context.Context, params *protocol.DocumentLink) (result *protocol.DocumentLink, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// DocumentSymbol implements protocol.Server.
func (s *Server) DocumentSymbol(ctx context.Context, params *protocol.DocumentSymbolParams) (result []interface{}, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// ExecuteCommand implements protocol.Server.
func (s *Server) ExecuteCommand(ctx context.Context, params *protocol.ExecuteCommandParams) (result interface{}, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// Exit implements protocol.Server.
func (s *Server) Exit(ctx context.Context) (err error) {
	return jsonrpc2.ErrMethodNotFound
}

// FoldingRanges implements protocol.Server.
func (s *Server) FoldingRanges(ctx context.Context, params *protocol.FoldingRangeParams) (result []protocol.FoldingRange, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// Formatting implements protocol.Server.
func (s *Server) Formatting(ctx context.Context, params *protocol.DocumentFormattingParams) (result []protocol.TextEdit, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// Implementation implements protocol.Server.
func (s *Server) Implementation(ctx context.Context, params *protocol.ImplementationParams) (result []protocol.Location, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// IncomingCalls implements protocol.Server.
func (s *Server) IncomingCalls(ctx context.Context, params *protocol.CallHierarchyIncomingCallsParams) (result []protocol.CallHierarchyIncomingCall, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// LinkedEditingRange implements protocol.Server.
func (s *Server) LinkedEditingRange(ctx context.Context, params *protocol.LinkedEditingRangeParams) (result *protocol.LinkedEditingRanges, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// LogTrace implements protocol.Server.
func (s *Server) LogTrace(ctx context.Context, params *protocol.LogTraceParams) (err error) {
	return jsonrpc2.ErrMethodNotFound
}

// Moniker implements protocol.Server.
func (s *Server) Moniker(ctx context.Context, params *protocol.MonikerParams) (result []protocol.Moniker, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// OnTypeFormatting implements protocol.Server.
func (s *Server) OnTypeFormatting(ctx context.Context, params *protocol.DocumentOnTypeFormattingParams) (result []protocol.TextEdit, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// OutgoingCalls implements protocol.Server.
func (s *Server) OutgoingCalls(ctx context.Context, params *protocol.CallHierarchyOutgoingCallsParams) (result []protocol.CallHierarchyOutgoingCall, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// PrepareCallHierarchy implements protocol.Server.
func (s *Server) PrepareCallHierarchy(ctx context.Context, params *protocol.CallHierarchyPrepareParams) (result []protocol.CallHierarchyItem, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// PrepareRename implements protocol.Server.
func (s *Server) PrepareRename(ctx context.Context, params *protocol.PrepareRenameParams) (result *protocol.Range, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// RangeFormatting implements protocol.Server.
func (s *Server) RangeFormatting(ctx context.Context, params *protocol.DocumentRangeFormattingParams) (result []protocol.TextEdit, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// References implements protocol.Server.
func (s *Server) References(ctx context.Context, params *protocol.ReferenceParams) (result []protocol.Location, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// Rename implements protocol.Server.
func (s *Server) Rename(ctx context.Context, params *protocol.RenameParams) (result *protocol.WorkspaceEdit, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// Request implements protocol.Server.
func (s *Server) Request(ctx context.Context, method string, params interface{}) (result interface{}, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// SemanticTokensFull implements protocol.Server.
func (s *Server) SemanticTokensFull(ctx context.Context, params *protocol.SemanticTokensParams) (result *protocol.SemanticTokens, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// SemanticTokensFullDelta implements protocol.Server.
func (s *Server) SemanticTokensFullDelta(ctx context.Context, params *protocol.SemanticTokensDeltaParams) (result interface{}, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// SemanticTokensRange implements protocol.Server.
func (s *Server) SemanticTokensRange(ctx context.Context, params *protocol.SemanticTokensRangeParams) (result *protocol.SemanticTokens, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// SemanticTokensRefresh implements protocol.Server.
func (s *Server) SemanticTokensRefresh(ctx context.Context) (err error) {
	return jsonrpc2.ErrMethodNotFound
}

// SetTrace implements protocol.Server.
func (s *Server) SetTrace(ctx context.Context, params *protocol.SetTraceParams) (err error) {
	return jsonrpc2.ErrMethodNotFound
}

// ShowDocument implements protocol.Server.
func (s *Server) ShowDocument(ctx context.Context, params *protocol.ShowDocumentParams) (result *protocol.ShowDocumentResult, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// Shutdown implements protocol.Server.
func (s *Server) Shutdown(ctx context.Context) (err error) {
	return jsonrpc2.ErrMethodNotFound
}

// SignatureHelp implements protocol.Server.
func (s *Server) SignatureHelp(ctx context.Context, params *protocol.SignatureHelpParams) (result *protocol.SignatureHelp, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// Symbols implements protocol.Server.
func (s *Server) Symbols(ctx context.Context, params *protocol.WorkspaceSymbolParams) (result []protocol.SymbolInformation, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// TypeDefinition implements protocol.Server.
func (s *Server) TypeDefinition(ctx context.Context, params *protocol.TypeDefinitionParams) (result []protocol.Location, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// WillCreateFiles implements protocol.Server.
func (s *Server) WillCreateFiles(ctx context.Context, params *protocol.CreateFilesParams) (result *protocol.WorkspaceEdit, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// WillDeleteFiles implements protocol.Server.
func (s *Server) WillDeleteFiles(ctx context.Context, params *protocol.DeleteFilesParams) (result *protocol.WorkspaceEdit, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// WillRenameFiles implements protocol.Server.
func (s *Server) WillRenameFiles(ctx context.Context, params *protocol.RenameFilesParams) (result *protocol.WorkspaceEdit, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// WillSave implements protocol.Server.
func (s *Server) WillSave(ctx context.Context, params *protocol.WillSaveTextDocumentParams) (err error) {
	return jsonrpc2.ErrMethodNotFound
}

// WillSaveWaitUntil implements protocol.Server.
func (s *Server) WillSaveWaitUntil(ctx context.Context, params *protocol.WillSaveTextDocumentParams) (result []protocol.TextEdit, err error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// WorkDoneProgressCancel implements protocol.Server.
func (s *Server) WorkDoneProgressCancel(ctx context.Context, params *protocol.WorkDoneProgressCancelParams) (err error) {
	return jsonrpc2.ErrMethodNotFound
}

var _ protocol.Server = &Server{}
