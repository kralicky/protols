// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// contains modified code from gopls/internal/golang/workspace_symbol.go.
package symbols

import (
	"sort"
	"strings"

	"github.com/kralicky/tools-lite/gopls/pkg/lsp/protocol"
	"github.com/kralicky/tools-lite/pkg/fuzzy"
)

const maxSymbols = 100

type SymbolStore[T any] interface {
	Store(SymbolInformation[T])
	TooLow(float64) bool
	Results() []protocol.SymbolInformation
}

func NewSymbolStore[T any](resolver func(SymbolInformation[T]) (protocol.SymbolInformation, bool)) SymbolStore[T] {
	return &symbolStore[T]{
		resolver: resolver,
	}
}

type symbolStore[T any] struct {
	resolver func(SymbolInformation[T]) (protocol.SymbolInformation, bool)
	res      [maxSymbols]SymbolInformation[T]
}

// Store inserts si into the sorted results, if si has a high enough score.
func (sc *symbolStore[T]) Store(si SymbolInformation[T]) {
	if sc.TooLow(si.Score) {
		return
	}
	insertAt := sort.Search(len(sc.res), func(i int) bool {
		// Sort by score, then symbol length, and finally lexically.
		if sc.res[i].Score != si.Score {
			return sc.res[i].Score < si.Score
		}
		if len(sc.res[i].Symbol) != len(si.Symbol) {
			return len(sc.res[i].Symbol) > len(si.Symbol)
		}
		return sc.res[i].Symbol > si.Symbol
	})
	if insertAt < len(sc.res)-1 {
		copy(sc.res[insertAt+1:], sc.res[insertAt:len(sc.res)-1])
	}
	sc.res[insertAt] = si
}

func (sc *symbolStore[T]) TooLow(score float64) bool {
	return score <= sc.res[len(sc.res)-1].Score
}

func (sc *symbolStore[T]) Results() []protocol.SymbolInformation {
	res := make([]protocol.SymbolInformation, 0, len(sc.res))
	for _, si := range sc.res {
		if si.Score <= 0 {
			return res
		}
		if resolved, ok := sc.resolver(si); ok {
			res = append(res, resolved)
		}
	}
	return res
}

type SymbolInformation[T any] struct {
	Score  float64
	Symbol string
	Data   T
}

// ParseQuery parses a field-separated symbol query, extracting the special
// characters listed below, and returns a matcherFunc corresponding to the AND
// of all field queries.
//
// Special characters:
//
//	^  match exact prefix
//	$  match exact suffix
//	'  match exact
//
// In all three of these special queries, matches are 'smart-cased', meaning
// they are case sensitive if the symbol query contains any upper-case
// characters, and case insensitive otherwise.
func ParseQuery(q string, newMatcher func(string) MatcherFunc) MatcherFunc {
	fields := strings.Fields(q)
	if len(fields) == 0 {
		return func([]string) (int, float64) { return -1, 0 }
	}
	var funcs []MatcherFunc
	for _, field := range fields {
		var f MatcherFunc
		switch {
		case strings.HasPrefix(field, "^"):
			prefix := field[1:]
			f = SmartCase(prefix, func(chunks []string) (int, float64) {
				s := strings.Join(chunks, "")
				if strings.HasPrefix(s, prefix) {
					return 0, 1
				}
				return -1, 0
			})
		case strings.HasPrefix(field, "'"):
			exact := field[1:]
			f = SmartCase(exact, MatchExact(exact))
		case strings.HasSuffix(field, "$"):
			suffix := field[0 : len(field)-1]
			f = SmartCase(suffix, func(chunks []string) (int, float64) {
				s := strings.Join(chunks, "")
				if strings.HasSuffix(s, suffix) {
					return len(s) - len(suffix), 1
				}
				return -1, 0
			})
		default:
			f = newMatcher(field)
		}
		funcs = append(funcs, f)
	}
	if len(funcs) == 1 {
		return funcs[0]
	}
	return ComboMatcher(funcs).Match
}

// A MatcherFunc returns the index and score of a symbol match.
//
// See the comment for symbolCollector for more information.
type MatcherFunc func(chunks []string) (int, float64)

// SmartCase returns a matcherFunc that is case-sensitive if q contains any
// upper-case characters, and case-insensitive otherwise.
func SmartCase(q string, m MatcherFunc) MatcherFunc {
	insensitive := strings.ToLower(q) == q
	wrapper := []string{""}
	return func(chunks []string) (int, float64) {
		s := strings.Join(chunks, "")
		if insensitive {
			s = strings.ToLower(s)
		}
		wrapper[0] = s
		return m(wrapper)
	}
}

func MatchExact(exact string) MatcherFunc {
	return func(chunks []string) (int, float64) {
		s := strings.Join(chunks, "")
		if idx := strings.LastIndex(s, exact); idx >= 0 {
			return idx, 1
		}
		return -1, 0
	}
}

type ComboMatcher []MatcherFunc

func (c ComboMatcher) Match(chunks []string) (int, float64) {
	score := 1.0
	first := 0
	for _, f := range c {
		idx, s := f(chunks)
		if idx < first {
			first = idx
		}
		score *= s
	}
	return first, score
}

func NewFuzzyMatcher(query string) MatcherFunc {
	fm := fuzzy.NewMatcher(query)
	return func(chunks []string) (int, float64) {
		score := float64(fm.ScoreChunks(chunks))
		ranges := fm.MatchedRanges()
		if len(ranges) > 0 {
			return ranges[0], score
		}
		return -1, score
	}
}
