package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/bufbuild/protocompile/ast"
	"github.com/bufbuild/protocompile/protoutil"
	"github.com/jhump/protoreflect/desc"
	"github.com/jhump/protoreflect/desc/protoprint"
	"github.com/samber/lo"
	"go.uber.org/zap"
	"golang.org/x/tools/gopls/pkg/lsp/protocol"
	"golang.org/x/tools/pkg/jsonrpc2"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type Server struct {
	lg *zap.Logger
	c  *Cache
}

func NewServer(lg *zap.Logger) *Server {
	return &Server{
		lg: lg,
	}
}

// Initialize implements protocol.Server.
func (s *Server) Initialize(ctx context.Context, params *protocol.ParamInitialize) (result *protocol.InitializeResult, err error) {
	folders := params.WorkspaceFolders
	if len(folders) != 1 {
		return nil, errors.New("multi-folder workspaces are not supported yet") // TODO
	}

	s.c = NewCache(params.RootURI.SpanURI().Filename(), s.lg.Named("cache"))

	filters := []protocol.FileOperationFilter{
		{
			Scheme: "file",
			Pattern: protocol.FileOperationPattern{
				Glob: "**/*.proto",
			},
		},
	}
	s.lg.Debug("Initialize", zap.String("workdir", params.RootURI.SpanURI().Filename()), zap.Any("initOpts", params.InitializationOptions))
	return &protocol.InitializeResult{
		Capabilities: protocol.ServerCapabilities{
			TextDocumentSync: protocol.TextDocumentSyncOptions{
				OpenClose: true,
				Change:    protocol.Incremental,
				// WillSaveWaitUntil: true,
				Save: &protocol.SaveOptions{IncludeText: false},
			},
			HoverProvider: &protocol.Or_ServerCapabilities_hoverProvider{Value: true},
			DiagnosticProvider: &protocol.Or_ServerCapabilities_diagnosticProvider{
				Value: protocol.DiagnosticOptions{
					WorkspaceDiagnostics:  false,
					InterFileDependencies: false,
				},
			},
			Workspace: &protocol.Workspace6Gn{
				FileOperations: &protocol.FileOperationOptions{
					DidCreate: &protocol.FileOperationRegistrationOptions{
						Filters: filters,
					},
					DidRename: &protocol.FileOperationRegistrationOptions{
						Filters: filters,
					},
					DidDelete: &protocol.FileOperationRegistrationOptions{
						Filters: filters,
					},
				},
			},
			InlayHintProvider:    true,
			DocumentLinkProvider: &protocol.DocumentLinkOptions{},
			DocumentFormattingProvider: &protocol.Or_ServerCapabilities_documentFormattingProvider{
				Value: protocol.DocumentFormattingOptions{},
			},
			// DocumentRangeFormattingProvider: &protocol.Or_ServerCapabilities_documentRangeFormattingProvider{
			// 	Value: protocol.DocumentRangeFormattingOptions{},
			// },
			// DeclarationProvider: &protocol.Or_ServerCapabilities_declarationProvider{Value: true},
			// TypeDefinitionProvider: true,
			// ReferencesProvider: true,
			// WorkspaceSymbolProvider: &protocol.Or_ServerCapabilities_workspaceSymbolProvider{Value: true},
			// CompletionProvider: protocol.CompletionOptions{
			// 	TriggerCharacters: []string{"."},
			// },
			DefinitionProvider: &protocol.Or_ServerCapabilities_definitionProvider{Value: true},
			SemanticTokensProvider: &protocol.SemanticTokensOptions{
				Legend: protocol.SemanticTokensLegend{
					TokenTypes:     semanticTokenTypes,
					TokenModifiers: semanticTokenModifiers,
				},
				Full:  &protocol.Or_SemanticTokensOptions_full{Value: true},
				Range: &protocol.Or_SemanticTokensOptions_range{Value: true},
			},
			// DocumentSymbolProvider: &protocol.Or_ServerCapabilities_documentSymbolProvider{Value: true},
		},

		ServerInfo: &protocol.PServerInfoMsg_initialize{
			Name:    "protols",
			Version: "0.0.1",
		},
	}, nil

}

// DidOpen implements protocol.Server.
func (s *Server) DidOpen(ctx context.Context, params *protocol.DidOpenTextDocumentParams) (err error) {
	s.c.OnFileOpened(params.TextDocument)
	return nil
}

// DidClose implements protocol.Server.
func (s *Server) DidClose(ctx context.Context, params *protocol.DidCloseTextDocumentParams) (err error) {
	s.c.OnFileClosed(params.TextDocument)
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

// Definition implements protocol.Server.
func (s *Server) Definition(ctx context.Context, params *protocol.DefinitionParams) (result []protocol.Location, err error) {
	// return nil, jsonrpc2.ErrMethodNotFound
	s.lg.Debug("Definition Request", zap.String("uri", string(params.TextDocument.URI)), zap.Int("line", int(params.Position.Line)), zap.Int("col", int(params.Position.Character)))
	desc, _, err := findRelevantDescriptorAtLocation(&params.TextDocumentPositionParams, s.c, s.lg)
	if err != nil {
		return nil, err
	}
	if desc == nil {
		return nil, nil
	}
	parentFile := desc.ParentFile()
	if parentFile == nil {
		return nil, errors.New("no parent file found for descriptor")
	}
	containingFileResolver, err := s.c.FindResultByPath(parentFile.Path())
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
		s.lg.Debug("definition is an import: ", zap.String("import", containingFileResolver.Path()))
	default:
		return nil, fmt.Errorf("unexpected descriptor type %T", desc)
	}

	info := containingFileResolver.AST().NodeInfo(node)
	uri, err := s.c.PathToURI(containingFileResolver.Path())
	if err != nil {
		return nil, err
	}
	return []protocol.Location{
		{
			URI: protocol.URIFromSpanURI(uri),
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
	// return s.c.ComputeHover(params.TextDocumentPositionParams)

	// todo: this implementation kinda sucks
	s.lg.Debug("Hover Request", zap.String("uri", string(params.TextDocument.URI)), zap.Int("line", int(params.Position.Line)), zap.Int("col", int(params.Position.Character)))
	d, rng, err := findRelevantDescriptorAtLocation(&params.TextDocumentPositionParams, s.c, s.lg)
	if err != nil {
		return nil, err
	}

	// special case: hovers for file imports
	if fd, ok := d.(protoreflect.FileImport); ok {
		return &protocol.Hover{
			Range: rng,
			Contents: protocol.MarkupContent{
				Kind:  protocol.Markdown,
				Value: fmt.Sprintf("```protobuf\nimport %q;\n```", fd.Path()),
			},
		}, nil
	}

	wrap, err := desc.WrapDescriptor(d)
	if err != nil {
		return nil, err
	}
	printer := protoprint.Printer{
		SortElements:       true,
		CustomSortFunction: SortElements,
		Indent:             "  ",
		Compact:            protoprint.CompactDefault,
	}
	str, err := printer.PrintProtoToString(wrap)
	if err != nil {
		return nil, err
	}

	return &protocol.Hover{
		Range: rng,
		Contents: protocol.MarkupContent{
			Kind:  protocol.Markdown,
			Value: fmt.Sprintf("```protobuf\n%s\n```", str),
		},
	}, nil
}

// DidChange implements protocol.Server.
func (s *Server) DidChange(ctx context.Context, params *protocol.DidChangeTextDocumentParams) (err error) {
	return s.c.OnFileModified(params.TextDocument, params.ContentChanges)
}

// DidCreateFiles implements protocol.Server.
func (s *Server) DidCreateFiles(ctx context.Context, params *protocol.CreateFilesParams) (err error) {
	return s.c.OnFilesCreated(params.Files)
}

// DidDeleteFiles implements protocol.Server.
func (s *Server) DidDeleteFiles(ctx context.Context, params *protocol.DeleteFilesParams) (err error) {
	return s.c.OnFilesDeleted(params.Files)
}

// DidRenameFiles implements protocol.Server.
func (s *Server) DidRenameFiles(ctx context.Context, params *protocol.RenameFilesParams) (err error) {
	return s.c.OnFilesRenamed(params.Files)
}

// DidSave implements protocol.Server.
func (s *Server) DidSave(ctx context.Context, params *protocol.DidSaveTextDocumentParams) (err error) {
	return s.c.OnFileSaved(params)
}

// SemanticTokensFull implements protocol.Server.
func (s *Server) SemanticTokensFull(ctx context.Context, params *protocol.SemanticTokensParams) (result *protocol.SemanticTokens, err error) {
	tokens, err := s.c.ComputeSemanticTokens(params.TextDocument)
	if err != nil {
		return nil, err
	}
	return &protocol.SemanticTokens{
		Data: tokens,
	}, nil
}

// SemanticTokensFullDelta implements protocol.Server.
func (s *Server) SemanticTokensFullDelta(ctx context.Context, params *protocol.SemanticTokensDeltaParams) (result interface{}, err error) {
	return nil, nil
}

// SemanticTokensRange implements protocol.Server.
func (s *Server) SemanticTokensRange(ctx context.Context, params *protocol.SemanticTokensRangeParams) (result *protocol.SemanticTokens, err error) {
	tokens, err := s.c.ComputeSemanticTokensRange(params.TextDocument, params.Range)
	if err != nil {
		return nil, err
	}
	return &protocol.SemanticTokens{
		Data: tokens,
	}, nil
}

// SemanticTokensRefresh implements protocol.Server.
func (s *Server) SemanticTokensRefresh(ctx context.Context) (err error) {
	return nil
}

// DocumentSymbol implements protocol.Server.
func (s *Server) DocumentSymbol(ctx context.Context, params *protocol.DocumentSymbolParams) (result []interface{}, err error) {
	symbols, err := s.c.DocumentSymbolsForFile(params.TextDocument)
	if err != nil {
		return nil, err
	}
	return lo.ToAnySlice(symbols), nil
}

var _ protocol.Server = &Server{}

var semanticTokenTypes = []string{
	string(protocol.NamespaceType),
	string(protocol.TypeType),
	string(protocol.ClassType),
	string(protocol.EnumType),
	string(protocol.InterfaceType),
	string(protocol.StructType),
	string(protocol.TypeParameterType),
	string(protocol.ParameterType),
	string(protocol.VariableType),
	string(protocol.PropertyType),
	string(protocol.EnumMemberType),
	string(protocol.EventType),
	string(protocol.FunctionType),
	string(protocol.MethodType),
	string(protocol.MacroType),
	string(protocol.KeywordType),
	string(protocol.ModifierType),
	string(protocol.CommentType),
	string(protocol.StringType),
	string(protocol.NumberType),
	string(protocol.RegexpType),
	string(protocol.OperatorType),
	string(protocol.DecoratorType),
}

var semanticTokenModifiers = []string{
	string(protocol.ModDeclaration),
	string(protocol.ModDefinition),
	string(protocol.ModReadonly),
	string(protocol.ModStatic),
	string(protocol.ModDeprecated),
	string(protocol.ModAbstract),
	string(protocol.ModAsync),
	string(protocol.ModModification),
	string(protocol.ModDocumentation),
	string(protocol.ModDefaultLibrary),
}

// Declaration implements protocol.Server.
func (*Server) Declaration(context.Context, *protocol.DeclarationParams) (*protocol.Or_textDocument_declaration, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// Diagnostic implements protocol.Server.
func (s *Server) Diagnostic(ctx context.Context, params *protocol.DocumentDiagnosticParams) (*protocol.Or_DocumentDiagnosticReport, error) {
	reports, err := s.c.ComputeDiagnosticReports(params.TextDocument.URI.SpanURI())
	if err != nil {
		s.lg.Error("failed to compute diagnostic reports", zap.Error(err))
		return nil, err
	}
	items := []protocol.Diagnostic{}
	for _, report := range reports {
		items = append(items, *report)
	}
	return &protocol.Or_DocumentDiagnosticReport{
		Value: protocol.RelatedFullDocumentDiagnosticReport{
			FullDocumentDiagnosticReport: protocol.FullDocumentDiagnosticReport{
				Kind:  string(protocol.DiagnosticFull),
				Items: items,
			},
		},
	}, nil
}

// DiagnosticRefresh implements protocol.Server.
func (*Server) DiagnosticRefresh(context.Context) error {
	return jsonrpc2.ErrMethodNotFound
}

// DiagnosticWorkspace implements protocol.Server.
func (*Server) DiagnosticWorkspace(context.Context, *protocol.WorkspaceDiagnosticParams) (*protocol.WorkspaceDiagnosticReport, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// DidChangeConfiguration implements protocol.Server.
func (*Server) DidChangeConfiguration(context.Context, *protocol.DidChangeConfigurationParams) error {
	return jsonrpc2.ErrMethodNotFound
}

// DidChangeNotebookDocument implements protocol.Server.
func (*Server) DidChangeNotebookDocument(context.Context, *protocol.DidChangeNotebookDocumentParams) error {
	return jsonrpc2.ErrMethodNotFound
}

// DidChangeWatchedFiles implements protocol.Server.
func (*Server) DidChangeWatchedFiles(context.Context, *protocol.DidChangeWatchedFilesParams) error {
	return jsonrpc2.ErrMethodNotFound
}

// DidChangeWorkspaceFolders implements protocol.Server.
func (*Server) DidChangeWorkspaceFolders(context.Context, *protocol.DidChangeWorkspaceFoldersParams) error {
	return jsonrpc2.ErrMethodNotFound
}

// DidCloseNotebookDocument implements protocol.Server.
func (*Server) DidCloseNotebookDocument(context.Context, *protocol.DidCloseNotebookDocumentParams) error {
	return jsonrpc2.ErrMethodNotFound
}

// DidOpenNotebookDocument implements protocol.Server.
func (*Server) DidOpenNotebookDocument(context.Context, *protocol.DidOpenNotebookDocumentParams) error {
	return jsonrpc2.ErrMethodNotFound
}

// DidSaveNotebookDocument implements protocol.Server.
func (*Server) DidSaveNotebookDocument(context.Context, *protocol.DidSaveNotebookDocumentParams) error {
	return jsonrpc2.ErrMethodNotFound
}

// DocumentColor implements protocol.Server.
func (*Server) DocumentColor(context.Context, *protocol.DocumentColorParams) ([]protocol.ColorInformation, error) {
	return nil, nil
}

// DocumentHighlight implements protocol.Server.
func (*Server) DocumentHighlight(context.Context, *protocol.DocumentHighlightParams) ([]protocol.DocumentHighlight, error) {
	return nil, nil
}

// DocumentLink implements protocol.Server.
func (s *Server) DocumentLink(ctx context.Context, params *protocol.DocumentLinkParams) ([]protocol.DocumentLink, error) {
	return s.c.ComputeDocumentLinks(params.TextDocument)
}

// ExecuteCommand implements protocol.Server.
func (*Server) ExecuteCommand(context.Context, *protocol.ExecuteCommandParams) (interface{}, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// Exit implements protocol.Server.
func (*Server) Exit(context.Context) error {
	return jsonrpc2.ErrMethodNotFound
}

// FoldingRange implements protocol.Server.
func (*Server) FoldingRange(context.Context, *protocol.FoldingRangeParams) ([]protocol.FoldingRange, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// Formatting implements protocol.Server.
func (s *Server) Formatting(ctx context.Context, params *protocol.DocumentFormattingParams) ([]protocol.TextEdit, error) {
	return s.c.FormatDocument(params.TextDocument, params.Options)
}

// Implementation implements protocol.Server.
func (*Server) Implementation(context.Context, *protocol.ImplementationParams) ([]protocol.Location, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// IncomingCalls implements protocol.Server.
func (*Server) IncomingCalls(context.Context, *protocol.CallHierarchyIncomingCallsParams) ([]protocol.CallHierarchyIncomingCall, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// InlayHint implements protocol.Server.
func (s *Server) InlayHint(ctx context.Context, params *protocol.InlayHintParams) ([]protocol.InlayHint, error) {
	return s.c.ComputeInlayHints(params.TextDocument, params.Range)
}

// InlayHintRefresh implements protocol.Server.
func (*Server) InlayHintRefresh(context.Context) error {
	return jsonrpc2.ErrMethodNotFound
}

// InlineValue implements protocol.Server.
func (*Server) InlineValue(context.Context, *protocol.InlineValueParams) ([]protocol.Or_InlineValue, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// InlineValueRefresh implements protocol.Server.
func (*Server) InlineValueRefresh(context.Context) error {
	return jsonrpc2.ErrMethodNotFound
}

// LinkedEditingRange implements protocol.Server.
func (*Server) LinkedEditingRange(context.Context, *protocol.LinkedEditingRangeParams) (*protocol.LinkedEditingRanges, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// Moniker implements protocol.Server.
func (*Server) Moniker(context.Context, *protocol.MonikerParams) ([]protocol.Moniker, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// NonstandardRequest implements protocol.Server.
func (*Server) NonstandardRequest(ctx context.Context, method string, params interface{}) (interface{}, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// OnTypeFormatting implements protocol.Server.
func (*Server) OnTypeFormatting(context.Context, *protocol.DocumentOnTypeFormattingParams) ([]protocol.TextEdit, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// OutgoingCalls implements protocol.Server.
func (*Server) OutgoingCalls(context.Context, *protocol.CallHierarchyOutgoingCallsParams) ([]protocol.CallHierarchyOutgoingCall, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// PrepareCallHierarchy implements protocol.Server.
func (*Server) PrepareCallHierarchy(context.Context, *protocol.CallHierarchyPrepareParams) ([]protocol.CallHierarchyItem, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// PrepareRename implements protocol.Server.
func (*Server) PrepareRename(context.Context, *protocol.PrepareRenameParams) (*protocol.Msg_PrepareRename2Gn, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// PrepareTypeHierarchy implements protocol.Server.
func (*Server) PrepareTypeHierarchy(context.Context, *protocol.TypeHierarchyPrepareParams) ([]protocol.TypeHierarchyItem, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// Progress implements protocol.Server.
func (*Server) Progress(context.Context, *protocol.ProgressParams) error {
	return jsonrpc2.ErrMethodNotFound
}

// RangeFormatting implements protocol.Server.
func (s *Server) RangeFormatting(ctx context.Context, params *protocol.DocumentRangeFormattingParams) ([]protocol.TextEdit, error) {
	return nil, jsonrpc2.ErrMethodNotFound
	// return s.c.FormatDocument(params.TextDocument, params.Options, params.Range)
}

// References implements protocol.Server.
func (*Server) References(context.Context, *protocol.ReferenceParams) ([]protocol.Location, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// Rename implements protocol.Server.
func (*Server) Rename(context.Context, *protocol.RenameParams) (*protocol.WorkspaceEdit, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// Resolve implements protocol.Server.
func (*Server) Resolve(context.Context, *protocol.InlayHint) (*protocol.InlayHint, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// ResolveCodeAction implements protocol.Server.
func (*Server) ResolveCodeAction(context.Context, *protocol.CodeAction) (*protocol.CodeAction, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// ResolveCodeLens implements protocol.Server.
func (*Server) ResolveCodeLens(context.Context, *protocol.CodeLens) (*protocol.CodeLens, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// ResolveCompletionItem implements protocol.Server.
func (*Server) ResolveCompletionItem(context.Context, *protocol.CompletionItem) (*protocol.CompletionItem, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// ResolveDocumentLink implements protocol.Server.
func (*Server) ResolveDocumentLink(context.Context, *protocol.DocumentLink) (*protocol.DocumentLink, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// ResolveWorkspaceSymbol implements protocol.Server.
func (*Server) ResolveWorkspaceSymbol(context.Context, *protocol.WorkspaceSymbol) (*protocol.WorkspaceSymbol, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// SelectionRange implements protocol.Server.
func (*Server) SelectionRange(context.Context, *protocol.SelectionRangeParams) ([]protocol.SelectionRange, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// SetTrace implements protocol.Server.
func (*Server) SetTrace(context.Context, *protocol.SetTraceParams) error {
	return jsonrpc2.ErrMethodNotFound
}

// Shutdown implements protocol.Server.
func (*Server) Shutdown(context.Context) error {
	return jsonrpc2.ErrMethodNotFound
}

// SignatureHelp implements protocol.Server.
func (*Server) SignatureHelp(context.Context, *protocol.SignatureHelpParams) (*protocol.SignatureHelp, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// Subtypes implements protocol.Server.
func (*Server) Subtypes(context.Context, *protocol.TypeHierarchySubtypesParams) ([]protocol.TypeHierarchyItem, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// Supertypes implements protocol.Server.
func (*Server) Supertypes(context.Context, *protocol.TypeHierarchySupertypesParams) ([]protocol.TypeHierarchyItem, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// Symbol implements protocol.Server.
func (*Server) Symbol(context.Context, *protocol.WorkspaceSymbolParams) ([]protocol.SymbolInformation, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// TypeDefinition implements protocol.Server.
func (*Server) TypeDefinition(context.Context, *protocol.TypeDefinitionParams) ([]protocol.Location, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// WillCreateFiles implements protocol.Server.
func (*Server) WillCreateFiles(context.Context, *protocol.CreateFilesParams) (*protocol.WorkspaceEdit, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// WillDeleteFiles implements protocol.Server.
func (*Server) WillDeleteFiles(context.Context, *protocol.DeleteFilesParams) (*protocol.WorkspaceEdit, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// WillRenameFiles implements protocol.Server.
func (*Server) WillRenameFiles(context.Context, *protocol.RenameFilesParams) (*protocol.WorkspaceEdit, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// WillSave implements protocol.Server.
func (*Server) WillSave(context.Context, *protocol.WillSaveTextDocumentParams) error {
	return jsonrpc2.ErrMethodNotFound
}

// WillSaveWaitUntil implements protocol.Server.
func (s *Server) WillSaveWaitUntil(ctx context.Context, params *protocol.WillSaveTextDocumentParams) ([]protocol.TextEdit, error) {
	return nil, jsonrpc2.ErrMethodNotFound
}

// WorkDoneProgressCancel implements protocol.Server.
func (*Server) WorkDoneProgressCancel(context.Context, *protocol.WorkDoneProgressCancelParams) error {
	return jsonrpc2.ErrMethodNotFound
}
