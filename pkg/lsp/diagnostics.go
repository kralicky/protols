package lsp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/bufbuild/protocompile/ast"
	"github.com/bufbuild/protocompile/linker"
	"github.com/bufbuild/protocompile/reporter"
	"golang.org/x/exp/slices"
	"golang.org/x/tools/gopls/pkg/lsp/protocol"
)

type ProtoDiagnostic struct {
	Pos      ast.SourcePosInfo
	Severity protocol.DiagnosticSeverity
	Error    error
	Tags     []protocol.DiagnosticTag
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
	ListenerFunc    = func(event DiagnosticEvent, path string, diagnostics ...*ProtoDiagnostic)
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

func tagsForError(err error) []protocol.DiagnosticTag {
	switch errors.Unwrap(err).(type) {
	case linker.ErrorUnusedImport:
		return []protocol.DiagnosticTag{protocol.Unnecessary}
	default:
		return []protocol.DiagnosticTag{}
	}
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

	fmt.Fprintf(os.Stderr, "[diagnostic] error: %s\n", err.Error())

	pos := err.GetPosition()
	filename := pos.Start().Filename

	dr.diagnosticsMu.Lock()
	dl, _ := dr.getOrCreateDiagnosticListLocked(filename)
	dr.diagnosticsMu.Unlock()

	newDiagnostic := &ProtoDiagnostic{
		Pos:      pos,
		Severity: protocol.SeverityError,
		Error:    err.Unwrap(),
		Tags:     tagsForError(err),
	}
	dl.Add(newDiagnostic)

	dr.listenerMu.RLock()
	if dr.listener != nil {
		dr.listener(DiagnosticEventAdd, filename, newDiagnostic)
	}
	dr.listenerMu.RUnlock()

	return nil // allow the compiler to continue
}

func (dr *DiagnosticHandler) HandleWarning(err reporter.ErrorWithPos) {
	if err == nil {
		return
	}

	fmt.Fprintf(os.Stderr, "[diagnostic] warning: %s\n", err.Error())

	pos := err.GetPosition()
	filename := pos.Start().Filename

	dr.diagnosticsMu.Lock()
	dl, _ := dr.getOrCreateDiagnosticListLocked(filename)
	dr.diagnosticsMu.Unlock()

	newDiagnostic := &ProtoDiagnostic{
		Pos:      pos,
		Severity: protocol.SeverityWarning,
		Error:    err.Unwrap(),
		Tags:     tagsForError(err),
	}
	dl.Add(newDiagnostic)

	dr.listenerMu.RLock()
	if dr.listener != nil {
		dr.listener(DiagnosticEventAdd, filename, newDiagnostic)
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

	fmt.Printf("[diagnostic] querying diagnostics for %s: (%d results)\n", path, len(res))
	return res, resultId, unchanged
}

func (dr *DiagnosticHandler) ClearDiagnosticsForPath(path string) {
	dr.diagnosticsMu.Lock()
	defer dr.diagnosticsMu.Unlock()
	var prev []*ProtoDiagnostic
	if dl, ok := dr.diagnostics[path]; ok {
		prev = dl.Clear()
	}

	fmt.Printf("[diagnostic] clearing %d diagnostics for %s\n", len(prev), path)

	dr.listenerMu.RLock()
	if dr.listener != nil {
		dr.listener(DiagnosticEventClear, path, prev...)
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
		callback(DiagnosticEventAdd, path, dl.Diagnostics...)
	}
	dr.listenerMu.RUnlock()

	dr.diagnosticsMu.RUnlock()

	<-ctx.Done()

	dr.listenerMu.Lock()
	dr.listener = nil
	dr.listenerMu.Unlock()
}
