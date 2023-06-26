package main

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/bufbuild/protocompile/ast"
	"github.com/bufbuild/protocompile/linker"
	"github.com/bufbuild/protocompile/reporter"
	gsync "github.com/kralicky/gpkg/sync"
	"go.uber.org/atomic"
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
		modified: atomic.NewBool(false),
	}
}

type DiagnosticList struct {
	lock        sync.RWMutex
	Diagnostics []*ProtoDiagnostic
	ResultId    string
}

func (dl *DiagnosticList) Add(d *ProtoDiagnostic) {
	dl.lock.Lock()
	defer dl.lock.Unlock()
	dl.Diagnostics = append(dl.Diagnostics, d)
	dl.resetResultId()
}

func (dl *DiagnosticList) Get(prevResultId ...string) (diagnostics []*ProtoDiagnostic, resultId string, unchanged bool) {
	dl.lock.RLock()
	defer dl.lock.RUnlock()
	if len(prevResultId) == 1 && dl.ResultId == prevResultId[0] {
		return []*ProtoDiagnostic{}, dl.ResultId, true
	}
	return dl.Diagnostics, dl.ResultId, false
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

type DiagnosticHandler struct {
	diagnostics gsync.Map[string, *DiagnosticList]
	modified    *atomic.Bool
}

func tagsForError(err error) []protocol.DiagnosticTag {
	switch errors.Unwrap(err).(type) {
	case linker.ErrorUnusedImport:
		return []protocol.DiagnosticTag{protocol.Unnecessary}
	default:
		return []protocol.DiagnosticTag{}
	}
}

func (dr *DiagnosticHandler) HandleError(err reporter.ErrorWithPos) error {
	if err == nil {
		return nil
	}

	fmt.Fprintf(os.Stderr, "[diagnostic] error: %s\n", err.Error())

	pos := err.GetPosition()
	filename := pos.Start().Filename

	empty := DiagnosticList{
		Diagnostics: []*ProtoDiagnostic{},
	}
	dl, _ := dr.diagnostics.LoadOrStore(filename, &empty)
	dl.Add(&ProtoDiagnostic{
		Pos:      pos,
		Severity: protocol.SeverityError,
		Error:    err.Unwrap(),
		Tags:     tagsForError(err),
	})

	dr.modified.CompareAndSwap(false, true)

	return nil // allow the compiler to continue
}

func (dr *DiagnosticHandler) HandleWarning(err reporter.ErrorWithPos) {
	if err == nil {
		return
	}

	fmt.Fprintf(os.Stderr, "[diagnostic] error: %s\n", err.Error())

	pos := err.GetPosition()
	filename := pos.Start().Filename

	empty := DiagnosticList{
		Diagnostics: []*ProtoDiagnostic{},
	}
	dl, _ := dr.diagnostics.LoadOrStore(filename, &empty)
	dl.Add(&ProtoDiagnostic{
		Pos:      pos,
		Severity: protocol.SeverityWarning,
		Error:    err.Unwrap(),
		Tags:     tagsForError(err),
	})

	dr.modified.CompareAndSwap(false, true)
}

func (dr *DiagnosticHandler) GetDiagnosticsForPath(path string, prevResultId ...string) ([]*ProtoDiagnostic, string, bool) {
	dl, ok := dr.diagnostics.Load(path)
	if !ok {
		return []*ProtoDiagnostic{}, "", false
	}
	return dl.Get(prevResultId...)

	// fmt.Printf("[diagnostic] querying diagnostics for %s: (%d results)\n", path, len(res))
	// return res, ok
}

func (dr *DiagnosticHandler) ClearDiagnosticsForPath(path string) {
	dl, ok := dr.diagnostics.Load(path)
	if !ok {
		return
	}
	dl.Clear()

	// fmt.Printf("[diagnostic] clearing %d diagnostics for %s\n", len(dr.diagnostics[path]), path)

}

func (dr *DiagnosticHandler) MaybeRange(setup func(), fn func(string, *DiagnosticList) bool) bool {
	if dr.modified.CompareAndSwap(true, false) {
		setup()
		dr.diagnostics.Range(fn)
		return true
	}
	return false
}
