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
	ast.Walk(node, v,
		ast.WithBefore(func(n ast.Node) error {
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

			return nil
		}),
		ast.WithAfter(func(n ast.Node) error {
			indentLevel--
			return nil
		}),
	)
	return buf.String()
}

type dumpVisitor struct {
	buf *bytes.Buffer
}

// VisitEditionNode implements ast.Visitor.
func (v *dumpVisitor) VisitEditionNode(node *ast.EditionNode) error {
	v.buf.WriteString(fmt.Sprintf("val=%q\n", node.Edition.AsString()))
	return nil
}

func maybe[T any](t *T) (_ T) {
	if t == nil {
		return
	}
	return *t
}

func (v *dumpVisitor) VisitArrayLiteralNode(node *ast.ArrayLiteralNode) error {
	v.buf.WriteString(fmt.Sprintf("len=%d\n", len(node.Elements)))
	return nil
}

func (v *dumpVisitor) VisitCompactOptionsNode(node *ast.CompactOptionsNode) error {
	v.buf.WriteString(fmt.Sprintf("len=%d\n", len(node.Options)))
	return nil
}

func (v *dumpVisitor) VisitCompoundIdentNode(node *ast.CompoundIdentNode) error {
	parts := make([]string, len(node.Components))
	for i, component := range node.Components {
		parts[i] = fmt.Sprintf("val=%q", component.Val)
	}

	v.buf.WriteString(fmt.Sprintf("parts=%v\n", parts))
	return nil
}

func (v *dumpVisitor) VisitCompoundStringLiteralNode(node *ast.CompoundStringLiteralNode) error {
	v.buf.WriteString(fmt.Sprintf("val=%s\n", node.Val))
	return nil
}

func (v *dumpVisitor) VisitEmptyDeclNode(node *ast.EmptyDeclNode) error {
	v.buf.WriteString("\n")
	return nil
}

func (v *dumpVisitor) VisitErrorNode(*ast.ErrorNode) error {
	v.buf.WriteString("\n")
	return nil
}

func (v *dumpVisitor) VisitEnumNode(node *ast.EnumNode) error {
	v.buf.WriteString(fmt.Sprintf("val=%q #decls=%d\n", node.Name.Val, len(node.Decls)))
	return nil
}

func (v *dumpVisitor) VisitEnumValueNode(node *ast.EnumValueNode) error {
	v.buf.WriteString(fmt.Sprintf("val=%q number=%d\n", node.Name.Val, node.Number.Value()))
	return nil
}

func (v *dumpVisitor) VisitExtendNode(node *ast.ExtendNode) error {
	v.buf.WriteString(fmt.Sprintf("extendee=%q #decls=%d\n", node.Extendee.AsIdentifier(), len(node.Decls)))
	return nil
}

func (v *dumpVisitor) VisitExtensionRangeNode(node *ast.ExtensionRangeNode) error {
	v.buf.WriteString(fmt.Sprintf("#ranges=%d\n", len(node.Ranges)))
	return nil
}

func (v *dumpVisitor) VisitFieldNode(node *ast.FieldNode) error {
	v.buf.WriteString(fmt.Sprintf("label=%q type=%q name=%q tag=%d", maybe(node.Label.KeywordNode).Val, node.FldType.AsIdentifier(), maybe(node.Name).Val, maybe(node.Tag).Val))
	if node.Options != nil {
		v.buf.WriteString(fmt.Sprintf(" #options=%d", len(node.Options.Options)))
	}
	v.buf.WriteString("\n")
	return nil
}

func (v *dumpVisitor) VisitFieldReferenceNode(node *ast.FieldReferenceNode) error {
	v.buf.WriteString(fmt.Sprintf("val=%q\n", node.Value()))
	return nil
}

func (v *dumpVisitor) VisitFileNode(node *ast.FileNode) error {
	if node.Syntax == nil {
		v.buf.WriteString(fmt.Sprintf("!syntax #decls=%d\n", len(node.Decls)))
	} else {
		v.buf.WriteString(fmt.Sprintf("syntax=%q #decls=%d\n", maybe(node.Syntax).Syntax.AsString(), len(node.Decls)))
	}
	return nil
}

func (v *dumpVisitor) VisitFloatLiteralNode(node *ast.FloatLiteralNode) error {
	v.buf.WriteString(fmt.Sprintf("val=%v\n", node.Val))
	return nil
}

func (v *dumpVisitor) VisitGroupNode(node *ast.GroupNode) error {
	v.buf.WriteString(fmt.Sprintf("label=%q name=%q tag=%d #decls=%d\n", maybe(node.Label.KeywordNode).Val, maybe(node.Name).Val, maybe(node.Tag).Val, len(node.Decls)))
	return nil
}

func (v *dumpVisitor) VisitIdentNode(node *ast.IdentNode) error {
	v.buf.WriteString(fmt.Sprintf("val=%q\n", node.Val))
	return nil
}

func (v *dumpVisitor) VisitIncompleteIdentNode(node *ast.IncompleteIdentNode) error {
	if node.IncompleteVal == nil {
		v.buf.WriteString("!val\n")
	} else {
		v.buf.WriteString(fmt.Sprintf("!val=%q\n", node.IncompleteVal.AsIdentifier()))
	}
	return nil
}

func (v *dumpVisitor) VisitImportNode(node *ast.ImportNode) error {
	v.buf.WriteString(fmt.Sprintf("name=%q\n", node.Name.AsString()))
	return nil
}

func (v *dumpVisitor) VisitKeywordNode(node *ast.KeywordNode) error {
	v.buf.WriteString(fmt.Sprintf("val=%q\n", node.Val))
	return nil
}

func (v *dumpVisitor) VisitMapFieldNode(node *ast.MapFieldNode) error {
	v.buf.WriteString(fmt.Sprintf("keytype=%q valuetype=%q name=%q tag=%d", node.MapType.KeyType.AsIdentifier(), node.MapType.ValueType.AsIdentifier(), maybe(node.Name).Val, maybe(node.Tag).Val))
	if node.Options != nil {
		v.buf.WriteString(fmt.Sprintf(" #options=%d", len(node.Options.Options)))
	}
	v.buf.WriteString("\n")
	return nil
}

func (v *dumpVisitor) VisitMapTypeNode(node *ast.MapTypeNode) error {
	v.buf.WriteString(fmt.Sprintf("keytype=%q valuetype=%q\n", maybe(node.KeyType).Val, node.ValueType.AsIdentifier()))
	return nil
}

func (v *dumpVisitor) VisitMessageFieldNode(node *ast.MessageFieldNode) error {
	v.buf.WriteString(fmt.Sprintf("name=%q val=%T\n", maybe(node.Name).Name.Value(), maybeValue(node.Val)))
	return nil
}

func (v *dumpVisitor) VisitMessageLiteralNode(node *ast.MessageLiteralNode) error {
	v.buf.WriteString(fmt.Sprintf("#elements=%d\n", len(node.Elements)))
	return nil
}

func (v *dumpVisitor) VisitMessageNode(node *ast.MessageNode) error {
	v.buf.WriteString(fmt.Sprintf("name=%q #decls=%d\n", maybe(node.Name).Val, len(node.Decls)))
	return nil
}

func (v *dumpVisitor) VisitNegativeIntLiteralNode(node *ast.NegativeIntLiteralNode) error {
	v.buf.WriteString(fmt.Sprintf("val=%d\n", node.Val))
	return nil
}

func (v *dumpVisitor) VisitOneofNode(node *ast.OneofNode) error {
	v.buf.WriteString(fmt.Sprintf("name=%q #decls=%d\n", maybe(node.Name).Val, len(node.Decls)))
	return nil
}

func (v *dumpVisitor) VisitOptionNameNode(node *ast.OptionNameNode) error {
	v.buf.WriteString(fmt.Sprintf("name=%q\n", StringForOptionName(node)))
	return nil
}

func (v *dumpVisitor) VisitOptionNode(node *ast.OptionNode) error {
	if node.IsIncomplete() {
		v.buf.WriteString("(!) ")
	}
	v.buf.WriteString(fmt.Sprintf("name=%q val=%T\n", StringForOptionName(node.Name), maybeValue(node.Val)))
	return nil
}

func (v *dumpVisitor) VisitPackageNode(node *ast.PackageNode) error {
	if node.IsIncomplete() {
		v.buf.WriteString("!name")
	} else {
		v.buf.WriteString(fmt.Sprintf("name=%q\n", node.Name.AsIdentifier()))
	}
	return nil
}

func (v *dumpVisitor) VisitPositiveUintLiteralNode(node *ast.PositiveUintLiteralNode) error {
	v.buf.WriteString(fmt.Sprintf("val=%d\n", node.Val))
	return nil
}

func (v *dumpVisitor) VisitRPCNode(node *ast.RPCNode) error {
	v.buf.WriteString(fmt.Sprintf("name=%q input=%q output=%q #decls=%d\n", maybe(node.Name).Val, maybe(node.Input).MessageType.AsIdentifier(), maybe(node.Output).MessageType.AsIdentifier(), len(node.Decls)))
	return nil
}

func (v *dumpVisitor) VisitRPCTypeNode(node *ast.RPCTypeNode) error {
	v.buf.WriteString(fmt.Sprintf("name=%q stream=%q\n", node.MessageType.AsIdentifier(), maybe(node.Stream).Val))
	return nil
}

func (v *dumpVisitor) VisitRangeNode(node *ast.RangeNode) error {
	if node.To != nil {
		if node.EndVal != nil {
			v.buf.WriteString(fmt.Sprintf("start=%d end=%d\n", node.StartVal.Value(), node.EndVal.Value()))
		} else {
			v.buf.WriteString(fmt.Sprintf("start=%d max=%q\n", node.StartVal.Value(), node.Max.Val))
		}
	} else {
		v.buf.WriteString(fmt.Sprintf("start=%d\n", node.StartVal.Value()))
	}
	return nil
}

func (v *dumpVisitor) VisitReservedNode(node *ast.ReservedNode) error {
	switch {
	case node.Ranges != nil:
		v.buf.WriteString(fmt.Sprintf("#ranges=%d\n", len(node.Ranges)))
	case node.Names != nil:
		v.buf.WriteString(fmt.Sprintf("#names=%d\n", len(node.Names)))
	case node.Identifiers != nil:
		v.buf.WriteString(fmt.Sprintf("#identifiers=%d\n", len(node.Identifiers)))
	default:
		v.buf.WriteString("(empty)\n")
	}
	return nil
}

func (v *dumpVisitor) VisitRuneNode(node *ast.RuneNode) error {
	v.buf.WriteString(fmt.Sprintf("rune=%q\n", string(node.Rune)))
	return nil
}

func (v *dumpVisitor) VisitServiceNode(node *ast.ServiceNode) error {
	v.buf.WriteString(fmt.Sprintf("name=%q #decls=%d\n", maybe(node.Name).Val, len(node.Decls)))
	return nil
}

func (v *dumpVisitor) VisitSignedFloatLiteralNode(node *ast.SignedFloatLiteralNode) error {
	v.buf.WriteString(fmt.Sprintf("val=%v\n", node.Val))
	return nil
}

func (v *dumpVisitor) VisitSpecialFloatLiteralNode(node *ast.SpecialFloatLiteralNode) error {
	v.buf.WriteString(fmt.Sprintf("val=%v\n", node.Val))
	return nil
}

func (v *dumpVisitor) VisitStringLiteralNode(node *ast.StringLiteralNode) error {
	v.buf.WriteString(fmt.Sprintf("val=%v\n", node.Val))
	return nil
}

func (v *dumpVisitor) VisitSyntaxNode(node *ast.SyntaxNode) error {
	v.buf.WriteString(fmt.Sprintf("syntax=%q\n", node.Syntax.AsString()))
	return nil
}

func (v *dumpVisitor) VisitUintLiteralNode(node *ast.UintLiteralNode) error {
	v.buf.WriteString(fmt.Sprintf("val=%v\n", node.Val))
	return nil
}

var _ ast.Visitor = (*dumpVisitor)(nil)

func maybeValue(val ast.ValueNode) any {
	if _, ok := val.(ast.NoSourceNode); ok {
		return "(invalid)"
	}
	vi := val.Value()
	if vi == nil {
		return "(nil)"
	}
	return vi
}
