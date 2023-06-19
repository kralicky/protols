package main

import (
	"sync"

	"github.com/bufbuild/protocompile/ast"
	"github.com/bufbuild/protocompile/reporter"
	"golang.org/x/tools/gopls/pkg/lsp/protocol"
)

type ProtoDiagnostic struct {
	Pos      ast.SourcePosInfo
	Severity protocol.DiagnosticSeverity
	Error    error
}

func NewDiagnosticHandler() *DiagnosticHandler {
	return &DiagnosticHandler{
		diagnostics: make(map[string][]*ProtoDiagnostic),
	}
}

type DiagnosticHandler struct {
	diagnosticsMu sync.Mutex
	diagnostics   map[string][]*ProtoDiagnostic
}

func (dr *DiagnosticHandler) HandleError(err reporter.ErrorWithPos) error {
	if err == nil {
		return nil
	}

	dr.diagnosticsMu.Lock()
	defer dr.diagnosticsMu.Unlock()

	pos := err.GetPosition()
	filename := pos.Start().Filename

	dr.diagnostics[filename] = append(dr.diagnostics[filename], &ProtoDiagnostic{
		Pos:      pos,
		Severity: protocol.SeverityError,
		Error:    err.Unwrap(),
	})

	return nil // allow the compiler to continue
}

func (dr *DiagnosticHandler) HandleWarning(err reporter.ErrorWithPos) {
	if err == nil {
		return
	}
	dr.diagnosticsMu.Lock()
	defer dr.diagnosticsMu.Unlock()

	pos := err.GetPosition()
	filename := pos.Start().Filename

	dr.diagnostics[filename] = append(dr.diagnostics[filename], &ProtoDiagnostic{
		Pos:      pos,
		Severity: protocol.SeverityWarning,
		Error:    err.Unwrap(),
	})
}

func (dr *DiagnosticHandler) GetDiagnosticsForPath(path string) ([]*ProtoDiagnostic, bool) {
	dr.diagnosticsMu.Lock()
	defer dr.diagnosticsMu.Unlock()

	res, ok := dr.diagnostics[path]
	return res, ok
}

func (dr *DiagnosticHandler) ClearDiagnosticsForPath(path string) {
	dr.diagnosticsMu.Lock()
	defer dr.diagnosticsMu.Unlock()

	delete(dr.diagnostics, path)
}
