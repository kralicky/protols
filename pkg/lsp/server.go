package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"

	"github.com/kralicky/protols/pkg/format"
	"github.com/kralicky/protols/pkg/sources"
	"github.com/samber/lo"
	"golang.org/x/sync/errgroup"
	"golang.org/x/tools/gopls/pkg/file"
	"golang.org/x/tools/gopls/pkg/lsp"
	"golang.org/x/tools/gopls/pkg/lsp/progress"
	"golang.org/x/tools/gopls/pkg/lsp/protocol"
	"golang.org/x/tools/pkg/jsonrpc2"
)

type Server struct {
	cachesMu sync.RWMutex
	caches   map[string]*Cache
	client   protocol.Client

	trackerMu sync.Mutex
	tracker   *progress.Tracker

	diagnosticStreamMu     sync.RWMutex
	diagnosticStreamCancel func()
}

func NewServer(client protocol.Client) *Server {
	executablePath, _ := os.Executable()

	slog.With(
		"path", executablePath,
		"pid", os.Getpid(),
	).Info("starting server")

	return &Server{
		caches:  map[string]*Cache{},
		client:  client,
		tracker: progress.NewTracker(client),
	}
}

// Initialize implements protocol.Server.
func (s *Server) Initialize(ctx context.Context, params *protocol.ParamInitialize) (result *protocol.InitializeResult, err error) {
	folders := params.WorkspaceFolders
	s.tracker.SetSupportsWorkDoneProgress(params.Capabilities.Window.WorkDoneProgress)
	s.cachesMu.Lock()
	for _, folder := range folders {
		path := protocol.URIFromURI(folder.URI).Path()
		slog.Info("adding workspace folder", "path", path)
		c := NewCache(folder)
		c.LoadFiles(sources.SearchDirs(path))
		s.caches[path] = c
	}
	s.cachesMu.Unlock()
	filters := []protocol.FileOperationFilter{
		{
			Scheme: "file",
			Pattern: protocol.FileOperationPattern{
				Glob: "**/*.proto",
			},
		},
	}
	slog.Debug("Initialize", "folders", folders)
	return &protocol.InitializeResult{
		Capabilities: protocol.ServerCapabilities{
			TextDocumentSync: protocol.TextDocumentSyncOptions{
				OpenClose: true,
				Change:    protocol.Incremental,
				Save:      &protocol.SaveOptions{IncludeText: false},
			},

			HoverProvider: &protocol.Or_ServerCapabilities_hoverProvider{Value: true},
			DiagnosticProvider: &protocol.Or_ServerCapabilities_diagnosticProvider{
				Value: protocol.DiagnosticOptions{
					WorkspaceDiagnostics:  true,
					InterFileDependencies: true,
				},
			},
			Workspace: &protocol.Workspace6Gn{
				WorkspaceFolders: &protocol.WorkspaceFolders5Gn{
					Supported:           true,
					ChangeNotifications: "workspace/didChangeWorkspaceFolders",
				},
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
			CompletionProvider: &protocol.CompletionOptions{
				TriggerCharacters: []string{".", "(", "["},
			},
			CodeActionProvider: &protocol.CodeActionOptions{
				CodeActionKinds: []protocol.CodeActionKind{
					protocol.QuickFix,
					protocol.Refactor,
					protocol.RefactorExtract,
				},
			},

			// DeclarationProvider: &protocol.Or_ServerCapabilities_declarationProvider{Value: true},
			// TypeDefinitionProvider: true,
			ReferencesProvider: &protocol.Or_ServerCapabilities_referencesProvider{Value: true},
			// WorkspaceSymbolProvider: &protocol.Or_ServerCapabilities_workspaceSymbolProvider{Value: true},

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

func (s *Server) CacheForURI(uri protocol.DocumentURI) (*Cache, error) {
	s.cachesMu.RLock()
	caches := maps.Clone(s.caches)
	s.cachesMu.RUnlock()
	u, err := url.Parse(string(uri))
	if err != nil {
		return nil, fmt.Errorf("invalid uri: %w", err)
	}
	if u.Fragment != "" {
		for _, c := range caches {
			if c.workspace.Name == u.Fragment {
				return c, nil
			}
		}
		return nil, fmt.Errorf("%w: workspace %s does not exist", jsonrpc2.ErrMethodNotFound, u.Fragment)
	}
	for path, c := range caches {
		if strings.HasPrefix(u.Path, path) {
			// special case: ignore ${workspaceFolder}/vendor
			if strings.HasPrefix(u.Path, path+"/vendor") {
				continue
			}
			return c, nil
		}
	}
	// worst case, use the first cache that tracks the given uri (todo: this can be improved)
	for _, c := range caches {
		if c.TracksURI(uri) {
			return c, nil
		}
	}
	return nil, fmt.Errorf("%w: uri %s does not belong to any workspace folder", jsonrpc2.ErrMethodNotFound, uri)
}

// Completion implements protocol.Server.
func (s *Server) Completion(ctx context.Context, params *protocol.CompletionParams) (result *protocol.CompletionList, err error) {
	c, err := s.CacheForURI(params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	return c.GetCompletions(params)
}

// Initialized implements protocol.Server.
func (s *Server) Initialized(ctx context.Context, params *protocol.InitializedParams) (err error) {
	slog.Debug("Initialized")
	return nil
}

// Definition implements protocol.Server.
func (s *Server) Definition(ctx context.Context, params *protocol.DefinitionParams) (result []protocol.Location, err error) {
	c, err := s.CacheForURI(params.TextDocument.URI)
	if err != nil {
		return nil, err
	}

	desc, _, err := c.FindTypeDescriptorAtLocation(params.TextDocumentPositionParams)
	if err != nil {
		return nil, err
	} else if desc == nil {
		return nil, nil
	}
	loc, err := c.FindDefinitionForTypeDescriptor(desc)
	if err != nil {
		return nil, err
	}
	return loc, nil
}

// Hover implements protocol.Server.
func (s *Server) Hover(ctx context.Context, params *protocol.HoverParams) (result *protocol.Hover, err error) {
	c, err := s.CacheForURI(params.TextDocument.URI)
	if err != nil {
		return nil, err
	}

	return c.ComputeHover(params.TextDocumentPositionParams)
}

// DidOpen implements protocol.Server.
func (s *Server) DidOpen(ctx context.Context, params *protocol.DidOpenTextDocumentParams) (err error) {
	c, err := s.CacheForURI(params.TextDocument.URI)
	if err != nil {
		return err
	}

	uri := params.TextDocument.URI
	if !uri.IsFile() {
		return nil
	}
	return c.DidModifyFiles(ctx, []file.Modification{
		{
			URI:        uri,
			Action:     file.Open,
			Version:    params.TextDocument.Version,
			Text:       []byte(params.TextDocument.Text),
			LanguageID: params.TextDocument.LanguageID,
		},
	})
}

// DidClose implements protocol.Server.
func (s *Server) DidClose(ctx context.Context, params *protocol.DidCloseTextDocumentParams) (err error) {
	c, err := s.CacheForURI(params.TextDocument.URI)
	if err != nil {
		return err
	}

	uri := params.TextDocument.URI
	if !uri.IsFile() {
		return nil
	}
	return c.DidModifyFiles(ctx, []file.Modification{
		{
			URI:     uri,
			Action:  file.Close,
			Version: -1,
			Text:    nil,
		},
	})
}

// DidChange implements protocol.Server.
func (s *Server) DidChange(ctx context.Context, params *protocol.DidChangeTextDocumentParams) (err error) {
	c, err := s.CacheForURI(params.TextDocument.URI)
	if err != nil {
		return err
	}

	uri := params.TextDocument.URI
	if !uri.IsFile() {
		return nil
	}
	text, err := c.ChangedText(ctx, uri, params.ContentChanges)
	if err != nil {
		return err
	}
	return c.DidModifyFiles(ctx, []file.Modification{{
		URI:     uri,
		Action:  file.Change,
		Version: params.TextDocument.Version,
		Text:    text,
	}})
}

// DidChangeWatchedFiles implements protocol.Server.
func (s *Server) DidChangeWatchedFiles(ctx context.Context, params *protocol.DidChangeWatchedFilesParams) error {
	mods := map[*Cache][]file.Modification{}
	for _, change := range params.Changes {
		uri := change.URI
		if !uri.IsFile() {
			continue
		}
		cache, err := s.CacheForURI(uri)
		if err != nil {
			continue
		}
		mods[cache] = append(mods[cache], file.Modification{
			URI:    uri,
			Action: lsp.ChangeTypeToFileAction(change.Type),
			OnDisk: true,
		})
	}
	for c, mods := range mods {
		if err := c.DidModifyFiles(ctx, mods); err != nil {
			slog.Error("failed to update files", "error", err)
		}
	}
	return nil
}

// DidCreateFiles implements protocol.Server.
func (s *Server) DidCreateFiles(ctx context.Context, params *protocol.CreateFilesParams) (err error) {
	modifications := map[*Cache][]file.Modification{}

	for _, f := range params.Files {
		uri := f.URI
		c, err := s.CacheForURI(protocol.DocumentURI(uri))
		if err != nil {
			return err
		}
		modifications[c] = append(modifications[c], file.Modification{
			URI:     protocol.URIFromURI(uri),
			Action:  file.Create,
			OnDisk:  true,
			Version: -1,
		})
	}
	for c, mods := range modifications {
		c.DidModifyFiles(ctx, mods)
	}
	return nil
}

// DidDeleteFiles implements protocol.Server.
func (s *Server) DidDeleteFiles(ctx context.Context, params *protocol.DeleteFilesParams) (err error) {
	modifications := map[*Cache][]file.Modification{}

	for _, f := range params.Files {
		uri := f.URI
		c, err := s.CacheForURI(protocol.DocumentURI(uri))
		if err != nil {
			return err
		}
		modifications[c] = append(modifications[c], file.Modification{
			URI:     protocol.URIFromURI(uri),
			Action:  file.Delete,
			Version: -1,
		})
	}
	for c, mods := range modifications {
		c.DidModifyFiles(ctx, mods)
	}
	return nil
}

// DidRenameFiles implements protocol.Server.
func (s *Server) DidRenameFiles(ctx context.Context, params *protocol.RenameFilesParams) (err error) {
	modifications := map[*Cache][]file.Modification{}

	for _, f := range params.Files {
		oldC, err := s.CacheForURI(protocol.DocumentURI(f.OldURI))
		if err != nil {
			return err
		}
		newC, err := s.CacheForURI(protocol.DocumentURI(f.NewURI))
		if err != nil {
			return err
		}
		modifications[oldC] = append(modifications[oldC], file.Modification{
			URI:     protocol.URIFromURI(f.OldURI),
			Action:  file.Delete,
			OnDisk:  true,
			Version: -1,
		})
		modifications[newC] = append(modifications[newC], file.Modification{
			URI:     protocol.URIFromURI(f.NewURI),
			Action:  file.Create,
			OnDisk:  true,
			Version: -1,
		})
	}
	for c, mods := range modifications {
		c.DidModifyFiles(ctx, mods)
	}
	return nil
}

// DidSave implements protocol.Server.
func (s *Server) DidSave(ctx context.Context, params *protocol.DidSaveTextDocumentParams) (err error) {
	c, err := s.CacheForURI(params.TextDocument.URI)
	if err != nil {
		return err
	}
	mod := file.Modification{
		URI:    params.TextDocument.URI,
		Action: file.Save,
	}
	if params.Text != nil {
		mod.Text = []byte(*params.Text)
	}
	return c.DidModifyFiles(ctx, []file.Modification{mod})
}

// SemanticTokensFull implements protocol.Server.
func (s *Server) SemanticTokensFull(ctx context.Context, params *protocol.SemanticTokensParams) (result *protocol.SemanticTokens, err error) {
	c, err := s.CacheForURI(params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	tokens, err := c.ComputeSemanticTokens(params.TextDocument)
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
	c, err := s.CacheForURI(params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	tokens, err := c.ComputeSemanticTokensRange(params.TextDocument, params.Range)
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
	c, err := s.CacheForURI(params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	symbols, err := c.DocumentSymbolsForFile(params.TextDocument)
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

// Diagnostic implements protocol.Server.
func (s *Server) Diagnostic(ctx context.Context, params *protocol.DocumentDiagnosticParams) (*protocol.Or_DocumentDiagnosticReport, error) {
	c, err := s.CacheForURI(params.TextDocument.URI)
	if err != nil {
		return nil, err
	}

	reports, kind, resultId, err := c.ComputeDiagnosticReports(params.TextDocument.URI, params.PreviousResultID)
	if err != nil {
		slog.Error("failed to compute diagnostic reports", "error", err)
		return nil, err
	}
	switch kind {
	case protocol.DiagnosticFull:
		return &protocol.Or_DocumentDiagnosticReport{
			Value: protocol.RelatedFullDocumentDiagnosticReport{
				FullDocumentDiagnosticReport: protocol.FullDocumentDiagnosticReport{
					Kind:     string(protocol.DiagnosticFull),
					ResultID: resultId,
					Items:    reports,
				},
			},
		}, nil
	case protocol.DiagnosticUnchanged:
		return &protocol.Or_DocumentDiagnosticReport{
			Value: protocol.RelatedUnchangedDocumentDiagnosticReport{
				UnchangedDocumentDiagnosticReport: protocol.UnchangedDocumentDiagnosticReport{
					Kind:     string(protocol.DiagnosticUnchanged),
					ResultID: resultId,
				},
			},
		}, nil
	default:
		panic("bug: unknown diagnostic kind: " + kind)
	}
}

// DiagnosticWorkspace implements protocol.Server.
func (s *Server) DiagnosticWorkspace(ctx context.Context, params *protocol.WorkspaceDiagnosticParams) (*protocol.WorkspaceDiagnosticReport, error) {
	if params.PartialResultToken == nil {
		return nil, jsonrpc2.ErrInvalidRequest
	}

	s.diagnosticStreamMu.RLock()
	if s.diagnosticStreamCancel != nil {
		s.diagnosticStreamCancel()
	}
	s.diagnosticStreamMu.RUnlock()

	s.diagnosticStreamMu.Lock()
	ctx, s.diagnosticStreamCancel = context.WithCancel(context.Background())
	s.diagnosticStreamMu.Unlock()

	s.cachesMu.RLock()
	caches := maps.Clone(s.caches)
	s.cachesMu.RUnlock()

	s.diagnosticStreamMu.RLock()
	defer s.diagnosticStreamMu.RUnlock()

	eg, ctx := errgroup.WithContext(ctx)
	reportsC := make(chan protocol.WorkspaceDiagnosticReportPartialResult, 100)
	for _, c := range caches {
		c := c
		eg.Go(func() error {
			c.StreamWorkspaceDiagnostics(ctx, reportsC)
			return nil
		})
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case report := <-reportsC:
				s.client.Progress(ctx, &protocol.ProgressParams{
					Token: params.PartialResultToken,
					Value: report,
				})
			}
		}
	}()
	eg.Wait()

	return &protocol.WorkspaceDiagnosticReport{
		Items: []protocol.Or_WorkspaceDocumentDiagnosticReport{},
	}, nil
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
	c, err := s.CacheForURI(params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	return c.ComputeDocumentLinks(params.TextDocument)
}

// Formatting implements protocol.Server.
func (s *Server) Formatting(ctx context.Context, params *protocol.DocumentFormattingParams) ([]protocol.TextEdit, error) {
	c, err := s.CacheForURI(params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	return c.FormatDocument(params.TextDocument, params.Options)
}

// InlayHint implements protocol.Server.
func (s *Server) InlayHint(ctx context.Context, params *protocol.InlayHintParams) ([]protocol.InlayHint, error) {
	c, err := s.CacheForURI(params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	return c.ComputeInlayHints(params.TextDocument, params.Range)
}

// NonstandardRequest implements protocol.Server.
func (s *Server) NonstandardRequest(ctx context.Context, method string, params interface{}) (interface{}, error) {
	switch method {
	case "protols/synthetic-file-contents":
		u := params.([]any)[0].(string)
		c, err := s.CacheForURI(protocol.DocumentURI(u))
		if err != nil {
			return nil, err
		}

		return c.GetSyntheticFileContents(ctx, u)
	case "protols/ast":
		u := params.([]any)[0].(string)
		c, err := s.CacheForURI(protocol.DocumentURI(u))
		if err != nil {
			return nil, err
		}
		parseRes, err := c.FindParseResultByURI(protocol.URIFromURI(u))
		if err != nil {
			return nil, err
		}
		return format.DumpAST(parseRes.AST(), parseRes), nil
	case "protols/reindex-workspaces":
		s.cachesMu.Lock()
		allWorkspaces := []protocol.WorkspaceFolder{}
		for _, c := range s.caches {
			allWorkspaces = append(allWorkspaces, c.workspace)
		}
		slog.Info("reindexing workspaces")
		clear(s.caches)
		runtime.GC()
		for _, folder := range allWorkspaces {
			path := protocol.URIFromURI(folder.URI).Path()
			c := NewCache(folder)
			c.LoadFiles(sources.SearchDirs(path))
			s.caches[path] = c
		}
		s.cachesMu.Unlock()
		return nil, nil
	default:
		return nil, fmt.Errorf("%w: unknown nonstandard request %q", jsonrpc2.ErrMethodNotFound, method)
	}
}

// References implements protocol.Server.
func (s *Server) References(ctx context.Context, params *protocol.ReferenceParams) ([]protocol.Location, error) {
	c, err := s.CacheForURI(params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	return c.FindReferences(ctx, params.TextDocumentPositionParams, params.Context)
}

// Shutdown implements protocol.Server.
func (*Server) Shutdown(context.Context) error {
	return nil
}

// DidChangeWorkspaceFolders implements protocol.Server.
func (s *Server) DidChangeWorkspaceFolders(ctx context.Context, params *protocol.DidChangeWorkspaceFoldersParams) error {
	added := params.Event.Added
	removed := params.Event.Removed
	s.cachesMu.Lock()
	for _, folder := range added {
		path := protocol.URIFromURI(folder.URI).Path()
		slog.Info("adding workspace folder", "path", path)
		c := NewCache(folder)
		c.LoadFiles(sources.SearchDirs(path))
		s.caches[path] = c
	}
	for _, folder := range removed {
		path := protocol.URIFromURI(folder.URI).Path()
		slog.Info("removing workspace folder", "path", path)
		delete(s.caches, path)
	}
	s.cachesMu.Unlock()
	return nil
}

// CodeAction implements protocol.Server.
func (s *Server) CodeAction(ctx context.Context, params *protocol.CodeActionParams) ([]protocol.CodeAction, error) {
	c, err := s.CacheForURI(params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	diagnostics := params.Context.Diagnostics
	var result []protocol.CodeAction
	for _, d := range diagnostics {
		if d.Data == nil {
			continue
		}
		data, err := d.Data.MarshalJSON()
		if err != nil {
			return nil, err
		}
		codeActions := CodeActions{}
		if err := json.Unmarshal(data, &codeActions); err != nil {
			return nil, err
		}
		result = append(result, c.ToProtocolCodeActions(codeActions.Items, &d)...)
	}
	return result, nil
}

// =====================
// Unimplemented Methods
// =====================

func notImplemented(method string) error {
	return fmt.Errorf("%w: %q not yet implemented", jsonrpc2.ErrMethodNotFound, method)
}

// Declaration implements protocol.Server.
func (*Server) Declaration(context.Context, *protocol.DeclarationParams) (*protocol.Or_textDocument_declaration, error) {
	return nil, notImplemented("Declaration")
}

// SignatureHelp implements protocol.Server.
func (*Server) SignatureHelp(context.Context, *protocol.SignatureHelpParams) (*protocol.SignatureHelp, error) {
	return nil, notImplemented("SignatureHelp")
}

// Subtypes implements protocol.Server.
func (*Server) Subtypes(context.Context, *protocol.TypeHierarchySubtypesParams) ([]protocol.TypeHierarchyItem, error) {
	return nil, notImplemented("Subtypes")
}

// Supertypes implements protocol.Server.
func (*Server) Supertypes(context.Context, *protocol.TypeHierarchySupertypesParams) ([]protocol.TypeHierarchyItem, error) {
	return nil, notImplemented("Supertypes")
}

// Symbol implements protocol.Server.
func (*Server) Symbol(context.Context, *protocol.WorkspaceSymbolParams) ([]protocol.SymbolInformation, error) {
	return nil, notImplemented("Symbol")
}

// TypeDefinition implements protocol.Server.
func (*Server) TypeDefinition(context.Context, *protocol.TypeDefinitionParams) ([]protocol.Location, error) {
	return nil, notImplemented("TypeDefinition")
}

// WillCreateFiles implements protocol.Server.
func (*Server) WillCreateFiles(context.Context, *protocol.CreateFilesParams) (*protocol.WorkspaceEdit, error) {
	return nil, notImplemented("WillCreateFiles")
}

// WillDeleteFiles implements protocol.Server.
func (*Server) WillDeleteFiles(context.Context, *protocol.DeleteFilesParams) (*protocol.WorkspaceEdit, error) {
	return nil, notImplemented("WillDeleteFiles")
}

// WillRenameFiles implements protocol.Server.
func (*Server) WillRenameFiles(context.Context, *protocol.RenameFilesParams) (*protocol.WorkspaceEdit, error) {
	return nil, notImplemented("WillRenameFiles")
}

// WillSave implements protocol.Server.
func (*Server) WillSave(context.Context, *protocol.WillSaveTextDocumentParams) error {
	return notImplemented("WillSave")
}

// WillSaveWaitUntil implements protocol.Server.
func (s *Server) WillSaveWaitUntil(ctx context.Context, params *protocol.WillSaveTextDocumentParams) ([]protocol.TextEdit, error) {
	return nil, notImplemented("WillSaveWaitUntil")
}

// WorkDoneProgressCancel implements protocol.Server.
func (*Server) WorkDoneProgressCancel(context.Context, *protocol.WorkDoneProgressCancelParams) error {
	return notImplemented("WorkDoneProgressCancel")
}

// CodeLens implements protocol.Server.
func (s *Server) CodeLens(ctx context.Context, params *protocol.CodeLensParams) (result []protocol.CodeLens, err error) {
	return nil, notImplemented("CodeLens")
}

// CodeLensRefresh implements protocol.Server.
func (s *Server) CodeLensRefresh(ctx context.Context) (err error) {
	return notImplemented("CodeLensRefresh")
}

// CodeLensResolve implements protocol.Server.
func (s *Server) CodeLensResolve(ctx context.Context, params *protocol.CodeLens) (result *protocol.CodeLens, err error) {
	return nil, notImplemented("CodeLensResolve")
}

// ColorPresentation implements protocol.Server.
func (s *Server) ColorPresentation(ctx context.Context, params *protocol.ColorPresentationParams) (result []protocol.ColorPresentation, err error) {
	return nil, notImplemented("ColorPresentation")
}

// CompletionResolve implements protocol.Server.
func (s *Server) CompletionResolve(ctx context.Context, params *protocol.CompletionItem) (result *protocol.CompletionItem, err error) {
	return nil, notImplemented("CompletionResolve")
}

// Rename implements protocol.Server.
func (*Server) Rename(context.Context, *protocol.RenameParams) (*protocol.WorkspaceEdit, error) {
	return nil, notImplemented("Rename")
}

// Resolve implements protocol.Server.
func (*Server) Resolve(context.Context, *protocol.InlayHint) (*protocol.InlayHint, error) {
	return nil, notImplemented("Resolve")
}

// ResolveCodeAction implements protocol.Server.
func (*Server) ResolveCodeAction(context.Context, *protocol.CodeAction) (*protocol.CodeAction, error) {
	return nil, notImplemented("ResolveCodeAction")
}

// ResolveCodeLens implements protocol.Server.
func (*Server) ResolveCodeLens(context.Context, *protocol.CodeLens) (*protocol.CodeLens, error) {
	return nil, notImplemented("ResolveCodeLens")
}

// ResolveCompletionItem implements protocol.Server.
func (*Server) ResolveCompletionItem(context.Context, *protocol.CompletionItem) (*protocol.CompletionItem, error) {
	return nil, notImplemented("ResolveCompletionItem")
}

// ResolveDocumentLink implements protocol.Server.
func (*Server) ResolveDocumentLink(context.Context, *protocol.DocumentLink) (*protocol.DocumentLink, error) {
	return nil, notImplemented("ResolveDocumentLink")
}

// ResolveWorkspaceSymbol implements protocol.Server.
func (*Server) ResolveWorkspaceSymbol(context.Context, *protocol.WorkspaceSymbol) (*protocol.WorkspaceSymbol, error) {
	return nil, notImplemented("ResolveWorkspaceSymbol")
}

// SelectionRange implements protocol.Server.
func (*Server) SelectionRange(context.Context, *protocol.SelectionRangeParams) ([]protocol.SelectionRange, error) {
	return nil, notImplemented("SelectionRange")
}

// SetTrace implements protocol.Server.
func (*Server) SetTrace(context.Context, *protocol.SetTraceParams) error {
	return notImplemented("SetTrace")
}

// OnTypeFormatting implements protocol.Server.
func (*Server) OnTypeFormatting(context.Context, *protocol.DocumentOnTypeFormattingParams) ([]protocol.TextEdit, error) {
	return nil, notImplemented("OnTypeFormatting")
}

// OutgoingCalls implements protocol.Server.
func (*Server) OutgoingCalls(context.Context, *protocol.CallHierarchyOutgoingCallsParams) ([]protocol.CallHierarchyOutgoingCall, error) {
	return nil, notImplemented("OutgoingCalls")
}

// PrepareCallHierarchy implements protocol.Server.
func (*Server) PrepareCallHierarchy(context.Context, *protocol.CallHierarchyPrepareParams) ([]protocol.CallHierarchyItem, error) {
	return nil, notImplemented("PrepareCallHierarchy")
}

// PrepareRename implements protocol.Server.
func (*Server) PrepareRename(context.Context, *protocol.PrepareRenameParams) (*protocol.Msg_PrepareRename2Gn, error) {
	return nil, notImplemented("PrepareRename")
}

// PrepareTypeHierarchy implements protocol.Server.
func (*Server) PrepareTypeHierarchy(context.Context, *protocol.TypeHierarchyPrepareParams) ([]protocol.TypeHierarchyItem, error) {
	return nil, notImplemented("PrepareTypeHierarchy")
}

// Progress implements protocol.Server.
func (*Server) Progress(context.Context, *protocol.ProgressParams) error {
	return notImplemented("Progress")
}

// RangeFormatting implements protocol.Server.
func (s *Server) RangeFormatting(ctx context.Context, params *protocol.DocumentRangeFormattingParams) ([]protocol.TextEdit, error) {
	return nil, notImplemented("RangeFormatting")
}

// InlayHintRefresh implements protocol.Server.
func (*Server) InlayHintRefresh(context.Context) error {
	return notImplemented("InlayHintRefresh")
}

// InlineValue implements protocol.Server.
func (*Server) InlineValue(context.Context, *protocol.InlineValueParams) ([]protocol.Or_InlineValue, error) {
	return nil, notImplemented("InlineValue")
}

// InlineValueRefresh implements protocol.Server.
func (*Server) InlineValueRefresh(context.Context) error {
	return notImplemented("InlineValueRefresh")
}

// LinkedEditingRange implements protocol.Server.
func (*Server) LinkedEditingRange(context.Context, *protocol.LinkedEditingRangeParams) (*protocol.LinkedEditingRanges, error) {
	return nil, notImplemented("LinkedEditingRange")
}

// Moniker implements protocol.Server.
func (*Server) Moniker(context.Context, *protocol.MonikerParams) ([]protocol.Moniker, error) {
	return nil, notImplemented("Moniker")
}

// Implementation implements protocol.Server.
func (*Server) Implementation(context.Context, *protocol.ImplementationParams) ([]protocol.Location, error) {
	return nil, notImplemented("Implementation")
}

// IncomingCalls implements protocol.Server.
func (*Server) IncomingCalls(context.Context, *protocol.CallHierarchyIncomingCallsParams) ([]protocol.CallHierarchyIncomingCall, error) {
	return nil, notImplemented("IncomingCalls")
}

// DidChangeConfiguration implements protocol.Server.
func (*Server) DidChangeConfiguration(context.Context, *protocol.DidChangeConfigurationParams) error {
	return notImplemented("DidChangeConfiguration")
}

// DidChangeNotebookDocument implements protocol.Server.
func (*Server) DidChangeNotebookDocument(context.Context, *protocol.DidChangeNotebookDocumentParams) error {
	return notImplemented("DidChangeNotebookDocument")
}

// DidCloseNotebookDocument implements protocol.Server.
func (*Server) DidCloseNotebookDocument(context.Context, *protocol.DidCloseNotebookDocumentParams) error {
	return notImplemented("DidCloseNotebookDocument")
}

// DidOpenNotebookDocument implements protocol.Server.
func (*Server) DidOpenNotebookDocument(context.Context, *protocol.DidOpenNotebookDocumentParams) error {
	return notImplemented("DidOpenNotebookDocument")
}

// DidSaveNotebookDocument implements protocol.Server.
func (*Server) DidSaveNotebookDocument(context.Context, *protocol.DidSaveNotebookDocumentParams) error {
	return notImplemented("DidSaveNotebookDocument")
}

// ExecuteCommand implements protocol.Server.
func (*Server) ExecuteCommand(context.Context, *protocol.ExecuteCommandParams) (interface{}, error) {
	return nil, notImplemented("ExecuteCommand")
}

// Exit implements protocol.Server.
func (*Server) Exit(context.Context) error {
	return notImplemented("Exit")
}

// FoldingRange implements protocol.Server.
func (*Server) FoldingRange(context.Context, *protocol.FoldingRangeParams) ([]protocol.FoldingRange, error) {
	return nil, notImplemented("FoldingRange")
}

// InlineCompletion implements protocol.Server.
func (*Server) InlineCompletion(context.Context, *protocol.InlineCompletionParams) (*protocol.Or_Result_textDocument_inlineCompletion, error) {
	return nil, notImplemented("InlineCompletion")
}

// RangesFormatting implements protocol.Server.
func (*Server) RangesFormatting(context.Context, *protocol.DocumentRangesFormattingParams) ([]protocol.TextEdit, error) {
	return nil, notImplemented("RangesFormatting")
}

// DiagnosticRefresh implements protocol.Server.
func (*Server) DiagnosticRefresh(context.Context) error {
	return notImplemented("DiagnosticRefresh")
}
