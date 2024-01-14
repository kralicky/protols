package lsp

import (
	"context"
	"sync"

	"github.com/kralicky/tools-lite/gopls/pkg/file"
	"github.com/kralicky/tools-lite/gopls/pkg/lsp/protocol"
)

type documentVersionQueue struct {
	mu       sync.Mutex
	versions map[protocol.DocumentURI]int32
	queue    map[protocol.DocumentURI]map[int32]chan struct{}
}

func newDocumentVersionQueue() *documentVersionQueue {
	return &documentVersionQueue{
		versions: make(map[protocol.DocumentURI]int32),
		queue:    make(map[protocol.DocumentURI]map[int32]chan struct{}),
	}
}

func (t *documentVersionQueue) Wait(ctx context.Context, uri protocol.DocumentURI, version int32) error {
	t.mu.Lock()
	if currentVersion, ok := t.versions[uri]; ok && currentVersion >= version {
		t.mu.Unlock()
		return nil
	}
	if _, ok := t.queue[uri]; !ok {
		t.queue[uri] = make(map[int32]chan struct{})
	}
	qc, ok := t.queue[uri][version]
	if !ok {
		qc = make(chan struct{})
		t.queue[uri][version] = qc
	}
	t.mu.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-qc:
		return nil
	}
}

func (t *documentVersionQueue) Update(modifications ...file.Modification) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, mod := range modifications {
		t.versions[mod.URI] = int32(mod.Version)
		for v, qc := range t.queue[mod.URI] {
			if mod.Version == -1 || v <= int32(mod.Version) {
				delete(t.queue[mod.URI], v)
				close(qc)
			}
		}
	}
}
