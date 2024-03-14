// Translation of go/expect/extract.go for protobuf sources
package expect

import (
	"bytes"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/scanner"

	"github.com/kralicky/protocompile/ast"
	"github.com/kralicky/protocompile/parser"
	"github.com/kralicky/protocompile/reporter"
	"google.golang.org/protobuf/proto"
)

const (
	commentStart    = "@"
	commentStartLen = len(commentStart)
)

// Identifier is the type for an identifier in an Note argument list.
type Identifier string

// Parse collects all the notes present in a file.
// If content is nil, the filename specified is read and parsed, otherwise the
// content is used and the filename is used for positions and error messages.
// Each comment whose text starts with @ is parsed as a comma-separated
// sequence of notes.
// See the package documentation for details about the syntax of those
// notes.
func Parse(filename string, content []byte) ([]*Note, error) {
	switch filepath.Ext(filename) {
	case ".proto":
		filenode, err := parser.Parse(filename, bytes.NewReader(content), reporter.NewHandler(nil), 0)
		if err != nil {
			return nil, err
		}
		return ExtractProto(filenode)
	}
	return nil, nil
}

func ExtractProto(fileNode *ast.FileNode) ([]*Note, error) {
	var notes []*Note
	fileInfo := proto.GetExtension(fileNode, ast.E_FileInfo).(*ast.FileInfo)
	for _, commentInfo := range fileInfo.Comments {
		_, comment := fileNode.GetItem(ast.Item(commentInfo.Index))
		if !comment.IsValid() {
			continue
		}
		text, adjust := getAdjustedNote(comment.RawText())
		if text == "" {
			continue
		}
		pos := comment.Start()
		adjustedPos := ast.SourcePos{
			Filename: pos.Filename,
			Line:     pos.Line,
			Col:      pos.Col + adjust,
			Offset:   pos.Offset + adjust,
		}
		parsed, err := parse(adjustedPos, text)
		if err != nil {
			return nil, err
		}
		for _, p := range parsed {
			p.Comment = comment
		}
		notes = append(notes, parsed...)
	}
	return notes, nil
}

func getAdjustedNote(text string) (string, int) {
	if strings.HasPrefix(text, "/*") {
		text = strings.TrimSuffix(text, "*/")
	}
	text = text[2:] // remove "//" or "/*" prefix

	// Allow notes to appear within comments.
	// For example:
	// "// //@mark()" is valid.
	// "// @mark()" is not valid.
	// "// /*@mark()*/" is not valid.
	var adjust int
	if i := strings.Index(text, commentStart); i > 2 {
		// Get the text before the commentStart.
		pre := text[i-2 : i]
		if pre != "//" {
			return "", 0
		}
		text = text[i:]
		adjust = i
	}
	if !strings.HasPrefix(text, commentStart) {
		return "", 0
	}
	text = text[commentStartLen:]
	return text, commentStartLen + adjust
}

// Note is a parsed note from an expect comment.
// It knows the position of the start of the comment, and the name and
// arguments that make up the note.
type Note struct {
	Pos     ast.SourcePos // The position at which the note identifier appears
	Comment ast.Comment   // The comment from which the note was extracted
	Name    string        // the name associated with the note
	Args    []any         // the arguments for the note
}

const invalidToken rune = 0

type tokens struct {
	scanner scanner.Scanner
	current rune
	err     error
	base    ast.SourcePos
}

func (t *tokens) Init(base ast.SourcePos, text string) *tokens {
	t.base = base
	t.scanner.Init(strings.NewReader(text))
	t.scanner.Mode = scanner.GoTokens
	t.scanner.Whitespace ^= 1 << '\n' // don't skip new lines
	t.scanner.Error = func(s *scanner.Scanner, msg string) {
		t.Errorf("%v", msg)
	}
	return t
}

func (t *tokens) Consume() string {
	t.current = invalidToken
	return t.scanner.TokenText()
}

func (t *tokens) Token() rune {
	if t.err != nil {
		return scanner.EOF
	}
	if t.current == invalidToken {
		t.current = t.scanner.Scan()
	}
	return t.current
}

func (t *tokens) Skip(r rune) int {
	i := 0
	for t.Token() == '\n' {
		t.Consume()
		i++
	}
	return i
}

func (t *tokens) TokenString() string {
	return scanner.TokenString(t.Token())
}

func (t *tokens) Pos() ast.SourcePos {
	return ast.SourcePos{
		Filename: t.base.Filename,
		Line:     t.base.Line,
		Col:      t.base.Col + t.scanner.Column - 1,
		Offset:   t.base.Offset + t.scanner.Offset,
	}
}

func (t *tokens) Errorf(msg string, args ...interface{}) {
	if t.err != nil {
		return
	}
	t.err = fmt.Errorf(msg, args...)
}

func parse(base ast.SourcePos, text string) ([]*Note, error) {
	t := new(tokens).Init(base, text)
	notes := parseComment(t)
	if t.err != nil {
		return nil, fmt.Errorf("%v:%s", t.Pos(), t.err)
	}
	return notes, nil
}

func parseComment(t *tokens) []*Note {
	var notes []*Note
	for {
		t.Skip('\n')
		switch t.Token() {
		case scanner.EOF:
			return notes
		case scanner.Ident:
			notes = append(notes, parseNote(t))
		default:
			t.Errorf("unexpected %s parsing comment, expect identifier", t.TokenString())
			return nil
		}
		switch t.Token() {
		case scanner.EOF:
			return notes
		case ',', '\n':
			t.Consume()
		default:
			t.Errorf("unexpected %s parsing comment, expect separator", t.TokenString())
			return nil
		}
	}
}

func parseNote(t *tokens) *Note {
	n := &Note{
		Pos:  t.Pos(),
		Name: t.Consume(),
	}

	switch t.Token() {
	case ',', '\n', scanner.EOF:
		// no argument list present
		return n
	case '(':
		n.Args = parseArgumentList(t)
		return n
	default:
		t.Errorf("unexpected %s parsing note", t.TokenString())
		return nil
	}
}

func parseArgumentList(t *tokens) []interface{} {
	args := []interface{}{} // @name() is represented by a non-nil empty slice.
	t.Consume()             // '('
	t.Skip('\n')
	for t.Token() != ')' {
		args = append(args, parseArgument(t))
		if t.Token() != ',' {
			break
		}
		t.Consume()
		t.Skip('\n')
	}
	if t.Token() != ')' {
		t.Errorf("unexpected %s parsing argument list", t.TokenString())
		return nil
	}
	t.Consume() // ')'
	return args
}

func parseArgument(t *tokens) interface{} {
	switch t.Token() {
	case scanner.Ident:
		v := t.Consume()
		switch v {
		case "true":
			return true
		case "false":
			return false
		case "nil":
			return nil
		case "re":
			if t.Token() != scanner.String && t.Token() != scanner.RawString {
				t.Errorf("re must be followed by string, got %s", t.TokenString())
				return nil
			}
			pattern, _ := strconv.Unquote(t.Consume()) // can't fail
			re, err := regexp.Compile(pattern)
			if err != nil {
				t.Errorf("invalid regular expression %s: %v", pattern, err)
				return nil
			}
			return re
		default:
			return Identifier(v)
		}

	case scanner.String, scanner.RawString:
		v, _ := strconv.Unquote(t.Consume()) // can't fail
		return v

	case scanner.Int:
		s := t.Consume()
		v, err := strconv.ParseInt(s, 0, 0)
		if err != nil {
			t.Errorf("cannot convert %v to int: %v", s, err)
		}
		return v

	case scanner.Float:
		s := t.Consume()
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			t.Errorf("cannot convert %v to float: %v", s, err)
		}
		return v

	case scanner.Char:
		t.Errorf("unexpected char literal %s", t.Consume())
		return nil

	default:
		t.Errorf("unexpected %s parsing argument", t.TokenString())
		return nil
	}
}
