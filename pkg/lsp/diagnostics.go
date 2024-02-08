package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kralicky/protocompile"
	"github.com/kralicky/protocompile/ast"
	"github.com/kralicky/protocompile/linker"
	"github.com/kralicky/protocompile/parser"
	"github.com/kralicky/protocompile/reporter"
	"github.com/kralicky/tools-lite/gopls/pkg/lsp/protocol"
)

func (c *Cache) ComputeDiagnosticReports(uri protocol.DocumentURI, prevResultId string) ([]protocol.Diagnostic, protocol.DocumentDiagnosticReportKind, string, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	var maybePrevResultId []string
	if prevResultId != "" {
		maybePrevResultId = append(maybePrevResultId, prevResultId)
	}
	path, err := c.resolver.URIToPath(uri)
	if err != nil {
		slog.With(
			"error", err,
			"uri", string(uri),
		).Error("failed to resolve uri to path")
		return nil, protocol.DiagnosticUnchanged, "", nil
	}
	rawReports, resultId, unchanged := c.diagHandler.GetDiagnosticsForPath(path, maybePrevResultId...)
	if unchanged {
		return nil, protocol.DiagnosticUnchanged, resultId, nil
	}
	protocolReports := c.toProtocolDiagnostics(rawReports)
	if protocolReports == nil {
		protocolReports = []protocol.Diagnostic{}
	}

	return protocolReports, protocol.DiagnosticFull, resultId, nil
}

func (c *Cache) toProtocolDiagnostics(rawReports []*ProtoDiagnostic) []protocol.Diagnostic {
	var reports []protocol.Diagnostic
	for _, rawReport := range rawReports {
		for i, info := range rawReport.RelatedInformation {
			u, err := c.resolver.PathToURI(string(info.Location.URI))
			if err == nil {
				rawReport.RelatedInformation[i].Location.URI = u
			}
		}
		if rawReport.Severity == protocol.SeverityWarning && rawReport.WerrorCategory != "" {
			// look up Werror debug pragma for this file
			shouldElevate := true
			if p, ok := c.FindPragmasByPath(protocompile.ResolvedPath(rawReport.Path)); ok {
				if dbg, ok := p.Lookup(PragmaDebug); ok {
					for _, v := range strings.Fields(dbg) {
						if k, v, ok := strings.Cut(v, "="); ok {
							if k == PragmaDebugWnoerror && (v == rawReport.WerrorCategory || v == WnoerrorAll) {
								shouldElevate = false
								break
							}
						}
					}
				}
			}
			if shouldElevate {
				rawReport.Severity = protocol.SeverityError
			}
		}
		report := protocol.Diagnostic{
			Range:              rawReport.Range,
			Severity:           rawReport.Severity,
			Message:            rawReport.Error.Error(),
			Tags:               rawReport.Tags,
			RelatedInformation: rawReport.RelatedInformation,
			Source:             "protols",
		}
		if rawReport.WerrorCategory != "" {
			report.Code = rawReport.WerrorCategory
		}
		data := DiagnosticData{
			Metadata:    rawReport.Metadata,
			CodeActions: rawReport.CodeActions,
		}
		jsonData, err := json.Marshal(data)
		if err != nil {
			panic(err)
		}
		rawMsg := json.RawMessage(jsonData)
		report.Data = &rawMsg
		reports = append(reports, report)
	}
	return reports
}

type workspaceDiagnosticCallbackFunc = func(uri protocol.DocumentURI, reports []protocol.Diagnostic, kind protocol.DocumentDiagnosticReportKind, resultId string)

func (c *Cache) StreamWorkspaceDiagnostics(ctx context.Context, ch chan<- protocol.WorkspaceDiagnosticReportPartialResult) {
	currentDiagnostics := make(map[protocol.DocumentURI][]protocol.Diagnostic)
	c.diagHandler.Stream(ctx, func(event DiagnosticEvent, path string, version int32, diagnostics ...*ProtoDiagnostic) {
		uri, err := c.resolver.PathToURI(path)
		if err != nil {
			return
		}

		switch event {
		case DiagnosticEventAdd:
			protocolDiagnostics := c.toProtocolDiagnostics(diagnostics)
			currentDiagnostics[uri] = append(currentDiagnostics[uri], protocolDiagnostics...)
			ch <- protocol.WorkspaceDiagnosticReportPartialResult{
				Items: []protocol.Or_WorkspaceDocumentDiagnosticReport{
					{
						Value: protocol.WorkspaceFullDocumentDiagnosticReport{
							Version: version,
							URI:     uri,
							FullDocumentDiagnosticReport: protocol.FullDocumentDiagnosticReport{
								Kind:  string(protocol.DiagnosticFull),
								Items: currentDiagnostics[uri],
							},
						},
					},
				},
			}
		case DiagnosticEventClear:
			delete(currentDiagnostics, uri)
			ch <- protocol.WorkspaceDiagnosticReportPartialResult{
				Items: []protocol.Or_WorkspaceDocumentDiagnosticReport{
					{
						Value: protocol.WorkspaceFullDocumentDiagnosticReport{
							Version: version,
							URI:     uri,
							FullDocumentDiagnosticReport: protocol.FullDocumentDiagnosticReport{
								Kind:  string(protocol.DiagnosticFull),
								Items: []protocol.Diagnostic{},
							},
						},
					},
				},
			}
		}
	})
}

type ProtoDiagnostic struct {
	Path               string
	Version            int32
	Range              protocol.Range
	Severity           protocol.DiagnosticSeverity
	Error              error
	Tags               []protocol.DiagnosticTag
	RelatedInformation []protocol.DiagnosticRelatedInformation
	CodeActions        []CodeAction
	Metadata           map[string]string

	// If this is a warning being treated as an error, WerrorCategory will be set to
	// a category that can be named in a debug pragma to disable it.
	WerrorCategory string
}

const (
	diagnosticKind               = "kind"
	diagnosticKindUndeclaredName = "undeclaredName"
	diagnosticKindUnusedImport   = "unusedImport"
)

type DiagnosticData struct {
	CodeActions []CodeAction      `json:"codeActions"`
	Metadata    map[string]string `json:"metadata"`
}

type CodeAction struct {
	Title       string                  `json:"title"`
	Path        string                  `json:"path,omitempty"`
	Kind        protocol.CodeActionKind `json:"kind,omitempty"`
	IsPreferred bool                    `json:"isPreferred,omitempty"`
	Edits       []protocol.TextEdit     `json:"edit,omitempty"`
	Command     *protocol.Command       `json:"command,omitempty"`
}

func NewDiagnosticHandler() *DiagnosticHandler {
	return &DiagnosticHandler{
		diagnostics: map[string]*DiagnosticList{},
	}
}

type DiagnosticList struct {
	lock        sync.RWMutex
	Diagnostics []*ProtoDiagnostic
	ResultId    string
}

func (dl *DiagnosticList) Add(d *ProtoDiagnostic) {
	if d == nil {
		panic("bug: DiagnosticList: attempted to add nil diagnostic")
	}
	dl.lock.Lock()
	defer dl.lock.Unlock()
	dl.Diagnostics = append(dl.Diagnostics, d)
	dl.resetResultId()
}

func (dl *DiagnosticList) Get(prevResultId ...string) (diagnostics []*ProtoDiagnostic, resultId string, unchanged bool) {
	dl.lock.RLock()
	defer dl.lock.RUnlock()
	return dl.getLocked(prevResultId...)
}

func (dl *DiagnosticList) getLocked(prevResultId ...string) (diagnostics []*ProtoDiagnostic, resultId string, unchanged bool) {
	if len(prevResultId) == 1 && dl.ResultId == prevResultId[0] {
		return []*ProtoDiagnostic{}, dl.ResultId, true
	}
	return slices.Clone(dl.Diagnostics), dl.ResultId, false
}

func (dl *DiagnosticList) Clear() []*ProtoDiagnostic {
	dl.lock.Lock()
	defer dl.lock.Unlock()
	dl.Diagnostics = []*ProtoDiagnostic{}
	dl.resetResultId()
	return dl.Diagnostics
}

// requires lock to be held in write mode
func (dl *DiagnosticList) resetResultId() {
	dl.ResultId = time.Now().Format(time.RFC3339Nano)
}

type (
	DiagnosticEvent int
	ListenerFunc    = func(event DiagnosticEvent, path string, version int32, diagnostics ...*ProtoDiagnostic)
)

const (
	DiagnosticEventAdd DiagnosticEvent = iota
	DiagnosticEventClear
)

type DiagnosticHandler struct {
	diagnosticsMu sync.RWMutex
	diagnostics   map[string]*DiagnosticList
	listenerMu    sync.RWMutex
	listener      ListenerFunc
}

func tagsForError(errWithPos reporter.ErrorWithPos) []protocol.DiagnosticTag {
	err := errWithPos.Unwrap()

	switch err.(type) {
	case linker.ErrorUnusedImport:
		return []protocol.DiagnosticTag{protocol.Unnecessary}
	default:
		return []protocol.DiagnosticTag{}
	}
}

func severityForError(original protocol.DiagnosticSeverity, errWithPos reporter.ErrorWithPos) protocol.DiagnosticSeverity {
	err := errWithPos.Unwrap()

	switch err.(type) {
	case linker.ErrorUnusedImport:
		return protocol.SeverityWarning
	default:
		return original
	}
}

func codeActionsForError(errWithPos reporter.ErrorWithPos) []CodeAction {
	err := errWithPos.Unwrap()
	pos := errWithPos.GetPosition()
	switch err := err.(type) {
	case linker.ErrorUnusedImport:
		return []CodeAction{
			{
				Title:       "Remove unused import",
				Kind:        protocol.SourceOrganizeImports,
				Path:        pos.Start().Filename,
				IsPreferred: true,
				Edits: []protocol.TextEdit{
					{
						// delete the line (column 0 of the current line to column 0 of the next line)
						Range: protocol.Range{
							Start: protocol.Position{
								Line:      uint32(pos.Start().Line - 1),
								Character: 0,
							},
							End: protocol.Position{
								Line:      uint32(pos.End().Line), // the next line
								Character: 0,
							},
						},
						NewText: "",
					},
				},
			},
		}
	case linker.ErrorUndeclaredName:
		name := err.UndeclaredName()
		if strings.Contains(name, ".") {
			break
		}
		f := err.ParentFile().(*ast.FileNode)
		tokens := f.Tokens()
		eof, _ := tokens.Last()
		last, _ := tokens.Previous(eof)
		info := f.TokenInfo(last)
		end := info.End()

		// insert the new message between the last non-comment token and the EOF token
		insertPos := protocol.Position{
			Line:      uint32(end.Line - 1),
			Character: uint32(end.Col - 1),
		}

		textToInsert := fmt.Sprintf("\n\nmessage %s {\n  \n}", name)
		// figure out where to trigger a range selection, after the text would be inserted
		revealRange := protocol.Range{
			Start: protocol.Position{
				Line:      uint32(end.Line + 1), // 2 lines after the end of the last token
				Character: 0,                    // column 0
			},
			End: protocol.Position{
				Line:      uint32(end.Line + 3),
				Character: 1,
			},
		}
		selectRange := protocol.Range{
			Start: protocol.Position{
				Line:      uint32(end.Line + 2),
				Character: 2,
			},
			End: protocol.Position{
				Line:      uint32(end.Line + 2),
				Character: 2,
			},
		}
		return []CodeAction{
			{
				Title: "Add definition for " + name + " in this file",
				Kind:  protocol.QuickFix,
				Path:  f.Name(),
				Edits: []protocol.TextEdit{
					{
						Range: protocol.Range{
							Start: insertPos,
							End:   insertPos,
						},
						NewText: textToInsert,
					},
				},
				Command: NewSelectRangeCommand(SelectRangeParams{
					SelectRange: selectRange,
					RevealRange: revealRange,
				}),
			},
		}
	}
	return []CodeAction{}
}

func metadataForError(errWithPos reporter.ErrorWithPos) map[string]string {
	err := errWithPos.Unwrap()
	switch err := err.(type) {
	case linker.ErrorUnusedImport:
		return map[string]string{
			diagnosticKind: diagnosticKindUnusedImport,
			"path":         err.UnusedImport(),
		}
	case linker.ErrorUndeclaredName:
		return map[string]string{
			diagnosticKind: diagnosticKindUndeclaredName,
			"name":         err.UndeclaredName(),
			"hint":         err.Hint(),
		}
	}
	return nil
}

func werrorCategoryForError(err error) string {
	var xse parser.ExtendedSyntaxError
	if errors.As(err, &xse) {
		return xse.Category()
	}
	return ""
}

func relatedInformationForError(err error) []protocol.DiagnosticRelatedInformation {
	var alreadyDefined reporter.AlreadyDefinedError
	if ok := errors.As(err, &alreadyDefined); ok {
		return []protocol.DiagnosticRelatedInformation{
			{
				Location: protocol.Location{
					URI:   protocol.DocumentURI(alreadyDefined.PreviousDefinition.Start().Filename),
					Range: toRange(alreadyDefined.PreviousDefinition),
				},
				Message: "previous definition",
			},
		}
	}
	return nil
}

func (dr *DiagnosticHandler) getOrCreateDiagnosticListLocked(filename string) (dl *DiagnosticList, existing bool) {
	dl, existing = dr.diagnostics[filename]
	if !existing {
		dl = &DiagnosticList{}
		dr.diagnostics[filename] = dl
	}
	return
}

func (dr *DiagnosticHandler) HandleError(err reporter.ErrorWithPos) error {
	if err == nil {
		return nil
	}

	slog.Debug(fmt.Sprintf("[diagnostic] error: %s\n", err.Error()))

	span := err.GetPosition()
	var version int32
	if info, ok := span.(ast.NodeInfo); ok {
		version = info.Internal().ParentFile().Version()
	}
	filename := span.Start().Filename

	dr.diagnosticsMu.Lock()
	dl, _ := dr.getOrCreateDiagnosticListLocked(filename)
	dr.diagnosticsMu.Unlock()

	newDiagnostic := &ProtoDiagnostic{
		Path:               filename,
		Version:            version,
		Range:              toRange(span),
		Severity:           protocol.SeverityError,
		Error:              err.Unwrap(),
		Tags:               tagsForError(err),
		RelatedInformation: relatedInformationForError(err),
		CodeActions:        codeActionsForError(err),
		Metadata:           metadataForError(err),
	}

	dl.Add(newDiagnostic)

	dr.listenerMu.RLock()
	if dr.listener != nil {
		dr.listener(DiagnosticEventAdd, filename, version, newDiagnostic)
	}
	dr.listenerMu.RUnlock()

	return nil // allow the compiler to continue
}

func (dr *DiagnosticHandler) HandleWarning(err reporter.ErrorWithPos) {
	if err == nil {
		return
	}

	slog.Debug(fmt.Sprintf("[diagnostic] warning: %s\n", err.Error()))

	span := err.GetPosition()
	var version int32
	if info, ok := span.(ast.NodeInfo); ok {
		version = info.Internal().ParentFile().Version()
	}
	filename := span.Start().Filename

	dr.diagnosticsMu.Lock()
	dl, _ := dr.getOrCreateDiagnosticListLocked(filename)
	dr.diagnosticsMu.Unlock()

	newDiagnostic := &ProtoDiagnostic{
		Path:               filename,
		Version:            version,
		Range:              toRange(span),
		Severity:           severityForError(protocol.SeverityWarning, err),
		Error:              err.Unwrap(),
		Tags:               tagsForError(err),
		CodeActions:        codeActionsForError(err),
		WerrorCategory:     werrorCategoryForError(err),
		RelatedInformation: relatedInformationForError(err),
		Metadata:           metadataForError(err),
	}
	dl.Add(newDiagnostic)

	dr.listenerMu.RLock()
	if dr.listener != nil {
		dr.listener(DiagnosticEventAdd, filename, version, newDiagnostic)
	}
	dr.listenerMu.RUnlock()
}

func (dr *DiagnosticHandler) GetDiagnosticsForPath(path string, prevResultId ...string) ([]*ProtoDiagnostic, string, bool) {
	dr.diagnosticsMu.RLock()
	defer dr.diagnosticsMu.RUnlock()
	dl, ok := dr.diagnostics[path]
	if !ok {
		return []*ProtoDiagnostic{}, "", false
	}
	res, resultId, unchanged := dl.Get(prevResultId...)

	slog.Debug(fmt.Sprintf("[diagnostic] querying diagnostics for %s: (%d results)\n", path, len(res)))
	return res, resultId, unchanged
}

func (dr *DiagnosticHandler) FullDiagnosticSnapshot() map[string][]*ProtoDiagnostic {
	dr.diagnosticsMu.RLock()
	defer dr.diagnosticsMu.RUnlock()
	res := make(map[string][]*ProtoDiagnostic, len(dr.diagnostics))
	for path, dl := range dr.diagnostics {
		list := make([]*ProtoDiagnostic, 0, len(dl.Diagnostics))
		for _, d := range dl.Diagnostics {
			list = append(list, &ProtoDiagnostic{
				Path:               d.Path,
				Version:            d.Version,
				Range:              d.Range,
				Severity:           d.Severity,
				Error:              d.Error,
				Tags:               d.Tags,
				RelatedInformation: d.RelatedInformation,
				CodeActions:        d.CodeActions,
				Metadata:           d.Metadata,
				WerrorCategory:     d.WerrorCategory,
			})
		}
		res[path] = list
	}
	return res
}

func (dr *DiagnosticHandler) ClearDiagnosticsForPath(path string) {
	dr.diagnosticsMu.Lock()
	defer dr.diagnosticsMu.Unlock()
	var prev []*ProtoDiagnostic
	if dl, ok := dr.diagnostics[path]; ok {
		prev = dl.Clear()
	}

	slog.Debug(fmt.Sprintf("[diagnostic] clearing %d diagnostics for %s\n", len(prev), path))

	var version int32
	if len(prev) > 0 {
		version = prev[len(prev)-1].Version
	}
	dr.listenerMu.RLock()
	if dr.listener != nil {
		dr.listener(DiagnosticEventClear, path, version, prev...)
	}
	dr.listenerMu.RUnlock()
}

func (dr *DiagnosticHandler) Stream(ctx context.Context, callback ListenerFunc) {
	dr.diagnosticsMu.RLock()

	dr.listenerMu.Lock()
	dr.listener = callback
	dr.listenerMu.Unlock()

	dr.listenerMu.RLock()
	for path, dl := range dr.diagnostics {
		var version int32 = 1
		if len(dl.Diagnostics) > 0 {
			version = max(version, dl.Diagnostics[len(dl.Diagnostics)-1].Version)
		}
		callback(DiagnosticEventAdd, path, version, dl.Diagnostics...)
	}
	dr.listenerMu.RUnlock()

	dr.diagnosticsMu.RUnlock()

	<-ctx.Done()

	dr.listenerMu.Lock()
	dr.listener = nil
	dr.listenerMu.Unlock()
}
