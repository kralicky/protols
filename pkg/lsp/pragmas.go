package lsp

import (
	"sync"

	"github.com/kralicky/protocompile/ast"
)

func init() {
	ast.PragmaKey = "protols"
}

const (
	PragmaNoFormat = "nofmt"
	PragmaDebug    = "debug"

	PragmaDebugWnoerror = "Wnoerror"
	WnoerrorAll         = "all"
)

type Pragmas interface {
	Lookup(key string) (value string, ok bool)
}

type pragmaMap struct {
	mu sync.RWMutex
	m  map[string]string
}

// Lookup implements Pragmas.
func (p *pragmaMap) Lookup(key string) (string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	v, ok := p.m[key]
	return v, ok
}

func (p *pragmaMap) update(m map[string]string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.m = m
}
