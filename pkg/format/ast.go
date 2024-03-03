package format

import (
	"bytes"
	"fmt"
	"math"
	"strings"

	"github.com/kralicky/protocompile/ast"
	"github.com/kralicky/protocompile/parser"
)

func DumpAST(node ast.Node, parseRes parser.Result) string {
	// print the ast for debugging purposes, with all the node types and hierarchy shown
	buf := new(bytes.Buffer)
	v := &dumpVisitor{buf: buf}
	indentLevel := -1
	fileNode := parseRes.AST()
	lineCount := fileNode.NodeInfo(fileNode).End().Line
	maxLeftPad := int(math.Log10(float64(lineCount)) + 1)
	ast.Inspect(node, v.Visit,
		ast.WithBefore(ast.NodeView(func(n ast.Node) {
			indentLevel++
			desc := parseRes.Descriptor(n)
			nodeInfo := fileNode.NodeInfo(n)
			line := nodeInfo.Start().Line
			fmt.Fprintf(buf, "%*d ", maxLeftPad, line)
			buf.WriteString(strings.Repeat("  ", indentLevel))
			leadingComments := nodeInfo.LeadingComments().Len()
			trailingComments := nodeInfo.TrailingComments().Len()

			if desc != nil {
				buf.WriteString(strings.Replace(fmt.Sprintf("[%T]{%T} ", n, desc), "*ast.", "", 1))
			} else {
				buf.WriteString(strings.Replace(fmt.Sprintf("[%T] ", n), "*ast.", "", 1))
			}
			if leadingComments > 0 {
				buf.WriteString(fmt.Sprintf("/lc=%d/ ", leadingComments))
			}
			if trailingComments > 0 {
				buf.WriteString(fmt.Sprintf("/tc=%d/ ", trailingComments))
			}
		})),
		ast.WithAfter(ast.NodeView(func(n ast.Node) {
			indentLevel--
		})),
	)
	return buf.String()
}

type dumpVisitor struct {
	buf *bytes.Buffer
}

func maybe[T any](t *T) (_ T) {
	if t == nil {
		return
	}
	return *t
}

func maybeValue(val *ast.ValueNode) any {
	vi := val.Value()
	if vi == nil {
		return "(nil)"
	}
	return vi
}

func (v *dumpVisitor) Visit(node ast.Node) bool {
	switch node := node.(type) {
	case *ast.EditionNode:
		v.buf.WriteString(fmt.Sprintf("val=%q\n", node.Edition.AsString()))

	case *ast.ArrayLiteralNode:
		v.buf.WriteString(fmt.Sprintf("len=%d\n", len(node.Elements)))

	case *ast.CompactOptionsNode:
		v.buf.WriteString(fmt.Sprintf("len=%d\n", len(node.Options)))

	case *ast.CompoundIdentNode:
		parts := make([]string, len(node.Components))
		for i, component := range node.Components {
			parts[i] = fmt.Sprintf("val=%q", component.Val)
		}
		v.buf.WriteString(fmt.Sprintf("parts=%v\n", parts))

	case *ast.CompoundStringLiteralNode:
		v.buf.WriteString(fmt.Sprintf("val=%s\n", node.AsString()))

	case *ast.EmptyDeclNode:
		v.buf.WriteString("\n")

	case *ast.ErrorNode:
		v.buf.WriteString("\n")

	case *ast.EnumNode:
		v.buf.WriteString(fmt.Sprintf("val=%q #decls=%d\n", node.Name.Val, len(node.Decls)))

	case *ast.EnumValueNode:
		v.buf.WriteString(fmt.Sprintf("val=%q number=%d\n", node.Name.Val, node.Number.Value()))

	case *ast.ExtendNode:
		if node.IsIncomplete() {
			v.buf.WriteString("!extendee\n")
		} else {
			v.buf.WriteString(fmt.Sprintf("extendee=%q #decls=%d\n", node.Extendee.AsIdentifier(), len(node.Decls)))
		}

	case *ast.ExtensionRangeNode:
		v.buf.WriteString(fmt.Sprintf("#elements=%d\n", len(node.Elements)))

	case *ast.FieldNode:
		v.buf.WriteString(fmt.Sprintf("label=%q type=%q name=%q tag=%d", maybe(node.Label).Val, node.FieldType.AsIdentifier(), maybe(node.Name).Val, maybe(node.Tag).Val))
		if node.Options != nil {
			v.buf.WriteString(fmt.Sprintf(" #options=%d", len(node.Options.Options)))
		}
		v.buf.WriteString("\n")

	case *ast.FieldReferenceNode:
		v.buf.WriteString(fmt.Sprintf("val=%q\n", node.Value()))

	case *ast.FileNode:
		if node.Syntax == nil {
			v.buf.WriteString(fmt.Sprintf("!syntax #decls=%d\n", len(node.Decls)))
		} else {
			v.buf.WriteString(fmt.Sprintf("syntax=%q #decls=%d\n", maybe(node.Syntax).Syntax.AsString(), len(node.Decls)))
		}

	case *ast.FloatLiteralNode:
		v.buf.WriteString(fmt.Sprintf("val=%v\n", node.Val))

	case *ast.GroupNode:
		v.buf.WriteString(fmt.Sprintf("label=%q name=%q tag=%d #decls=%d\n", maybe(node.Label).Val, maybe(node.Name).Val, maybe(node.Tag).Val, len(node.Decls)))

	case *ast.IdentNode:
		if node.IsKeyword {
			v.buf.WriteString(fmt.Sprintf("keyword=%q\n", node.Val))
		} else {
			v.buf.WriteString(fmt.Sprintf("val=%q\n", node.Val))
		}

	case *ast.ImportNode:
		if node.IsIncomplete() {
			v.buf.WriteString("!name\n")
		} else {
			if node.Public != nil {
				v.buf.WriteString("public ")
			}
			if node.Weak != nil {
				v.buf.WriteString("weak ")
			}
			v.buf.WriteString(fmt.Sprintf("name=%q\n", node.Name.AsString()))
		}

	case *ast.MapFieldNode:
		v.buf.WriteString(fmt.Sprintf("keytype=%q valuetype=%q name=%q tag=%d", node.MapType.KeyType.AsIdentifier(), node.MapType.ValueType.AsIdentifier(), maybe(node.Name).Val, maybe(node.Tag).Val))
		if node.Options != nil {
			v.buf.WriteString(fmt.Sprintf(" #options=%d", len(node.Options.Options)))
		}
		v.buf.WriteString("\n")

	case *ast.MapTypeNode:
		v.buf.WriteString(fmt.Sprintf("keytype=%q valuetype=%q\n", maybe(node.KeyType).Val, node.ValueType.AsIdentifier()))

	case *ast.MessageFieldNode:
		v.buf.WriteString(fmt.Sprintf("name=%q val=%T\n", maybe(node.Name).Name.AsIdentifier(), maybeValue(node.Val)))

	case *ast.MessageLiteralNode:
		v.buf.WriteString(fmt.Sprintf("#elements=%d\n", len(node.Elements)))

	case *ast.MessageNode:
		v.buf.WriteString(fmt.Sprintf("name=%q #decls=%d\n", maybe(node.Name).Val, len(node.Decls)))

	case *ast.NegativeIntLiteralNode:
		v.buf.WriteString(fmt.Sprintf("val=%d\n", node.Value()))

	case *ast.OneofNode:
		v.buf.WriteString(fmt.Sprintf("name=%q #decls=%d\n", maybe(node.Name).Val, len(node.Decls)))

	case *ast.OptionNameNode:
		v.buf.WriteString(fmt.Sprintf("name=%q\n", StringForOptionName(node)))

	case *ast.OptionNode:
		if node.IsIncomplete() {
			v.buf.WriteString("(!) ")
		}
		v.buf.WriteString(fmt.Sprintf("name=%q val=%T\n", StringForOptionName(node.Name), maybeValue(node.Val)))

	case *ast.PackageNode:
		if node.IsIncomplete() {
			v.buf.WriteString("!name")
		} else {
			v.buf.WriteString(fmt.Sprintf("name=%q\n", node.Name.AsIdentifier()))
		}

	case *ast.RPCNode:
		v.buf.WriteString(fmt.Sprintf("name=%q input=%q output=%q #decls=%d\n", maybe(node.Name).Val, maybe(node.Input).MessageType.AsIdentifier(), maybe(node.Output).MessageType.AsIdentifier(), len(node.Decls)))

	case *ast.RPCTypeNode:
		v.buf.WriteString(fmt.Sprintf("name=%q stream=%q\n", node.MessageType.AsIdentifier(), maybe(node.Stream).Val))

	case *ast.RangeNode:
		if node.To != nil {
			if node.EndVal != nil {
				v.buf.WriteString(fmt.Sprintf("start=%d end=%d\n", node.StartVal.Value(), node.EndVal.Value()))
			} else {
				v.buf.WriteString(fmt.Sprintf("start=%d max=%q\n", node.StartVal.Value(), node.Max.Val))
			}
		} else {
			v.buf.WriteString(fmt.Sprintf("start=%d\n", node.StartVal.Value()))
		}

	case *ast.ReservedNode:
		v.buf.WriteString(fmt.Sprintf("#elements=%d\n", len(node.Elements)))

	case *ast.RuneNode:
		v.buf.WriteString(fmt.Sprintf("rune=%q\n", string(node.Rune)))

	case *ast.ServiceNode:
		v.buf.WriteString(fmt.Sprintf("name=%q #decls=%d\n", maybe(node.Name).Val, len(node.Decls)))

	case *ast.SignedFloatLiteralNode:
		v.buf.WriteString(fmt.Sprintf("val=%v\n", node.AsFloat()))

	case *ast.SpecialFloatLiteralNode:
		v.buf.WriteString(fmt.Sprintf("val=%v\n", node.Val))

	case *ast.StringLiteralNode:
		v.buf.WriteString(fmt.Sprintf("val=%v\n", node.Val))

	case *ast.SyntaxNode:
		v.buf.WriteString(fmt.Sprintf("syntax=%q\n", node.Syntax.AsString()))

	case *ast.UintLiteralNode:
		v.buf.WriteString(fmt.Sprintf("val=%v\n", node.Val))
	}
	return true
}
