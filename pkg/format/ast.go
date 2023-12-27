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
			line := fileNode.NodeInfo(n).Start().Line
			fmt.Fprintf(buf, "%*d ", maxLeftPad, line)
			buf.WriteString(strings.Repeat("  ", indentLevel))
			if desc != nil {
				buf.WriteString(strings.Replace(fmt.Sprintf("[%T]{%T} ", n, desc), "*ast.", "", 1))
			} else {
				buf.WriteString(strings.Replace(fmt.Sprintf("[%T] ", n), "*ast.", "", 1))
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
	v.buf.WriteString(fmt.Sprintf("(val=%s)\n", node.Edition.AsString()))
	return nil
}

func maybe[T any](t *T) (_ T) {
	if t == nil {
		return
	}
	return *t
}

func (v *dumpVisitor) VisitArrayLiteralNode(node *ast.ArrayLiteralNode) error {
	v.buf.WriteString(fmt.Sprintf("(len=%d)\n", len(node.Elements)))
	return nil
}

func (v *dumpVisitor) VisitCompactOptionsNode(node *ast.CompactOptionsNode) error {
	v.buf.WriteString(fmt.Sprintf("(len=%d)\n", len(node.Options)))
	return nil
}

func (v *dumpVisitor) VisitCompoundIdentNode(node *ast.CompoundIdentNode) error {
	parts := make([]string, len(node.Components))
	for i, component := range node.Components {
		parts[i] = fmt.Sprintf("(val=%s)", component.Val)
	}

	v.buf.WriteString(fmt.Sprintf("(parts=%v)\n", parts))
	return nil
}

func (v *dumpVisitor) VisitCompoundStringLiteralNode(node *ast.CompoundStringLiteralNode) error {
	v.buf.WriteString(fmt.Sprintf("(val=%v)\n", node.Val))
	return nil
}

func (v *dumpVisitor) VisitEmptyDeclNode(node *ast.EmptyDeclNode) error {
	return nil
}

func (v *dumpVisitor) VisitEnumNode(node *ast.EnumNode) error {
	v.buf.WriteString(fmt.Sprintf("(val=%s) (#decls=%d)\n", node.Name.Val, len(node.Decls)))
	return nil
}

func (v *dumpVisitor) VisitEnumValueNode(node *ast.EnumValueNode) error {
	v.buf.WriteString(fmt.Sprintf("(val=%s) (number=%d)\n", node.Name.Val, node.Number.Value()))
	return nil
}

func (v *dumpVisitor) VisitExtendNode(node *ast.ExtendNode) error {
	return nil
}

func (v *dumpVisitor) VisitExtensionRangeNode(node *ast.ExtensionRangeNode) error {
	return nil
}

func (v *dumpVisitor) VisitFieldNode(node *ast.FieldNode) error {
	v.buf.WriteString(fmt.Sprintf("(label=%s) (type=%s) (name=%s) (tag=%d) (#options=%d)\n", maybe(node.Label.KeywordNode).Val, node.FldType.AsIdentifier(), maybe(node.Name).Val, maybe(node.Tag).Val, len(maybe(node.Options).Options)))
	return nil
}

func (v *dumpVisitor) VisitFieldReferenceNode(node *ast.FieldReferenceNode) error {
	v.buf.WriteString(fmt.Sprintf("(val=%s)\n", node.Value()))
	return nil
}

func (v *dumpVisitor) VisitFileNode(node *ast.FileNode) error {
	v.buf.WriteString(fmt.Sprintf("(syntax=%s) (#decls=%d)\n", maybe(node.Syntax).Syntax, len(node.Decls)))
	return nil
}

func (v *dumpVisitor) VisitFloatLiteralNode(node *ast.FloatLiteralNode) error {
	v.buf.WriteString(fmt.Sprintf("(val=%v)\n", node.Val))
	return nil
}

func (v *dumpVisitor) VisitGroupNode(node *ast.GroupNode) error {
	return nil
}

func (v *dumpVisitor) VisitIdentNode(node *ast.IdentNode) error {
	v.buf.WriteString(fmt.Sprintf("(val=%s)\n", node.Val))
	return nil
}

func (v *dumpVisitor) VisitIncompleteIdentNode(node *ast.IncompleteIdentNode) error {
	if node.IncompleteVal == nil {
		v.buf.WriteString("!(val=)\n")
	} else {
		v.buf.WriteString(fmt.Sprintf("!(val=%s)\n", node.IncompleteVal.AsIdentifier()))
	}
	return nil
}

func (v *dumpVisitor) VisitImportNode(node *ast.ImportNode) error {
	v.buf.WriteString(fmt.Sprintf("(name=%s)\n", node.Name.AsString()))
	return nil
}

func (v *dumpVisitor) VisitKeywordNode(node *ast.KeywordNode) error {
	v.buf.WriteString(fmt.Sprintf("(val=%s)\n", node.Val))
	return nil
}

func (v *dumpVisitor) VisitMapFieldNode(node *ast.MapFieldNode) error {
	v.buf.WriteString(fmt.Sprintf("(keytype=%s) (valuetype=%s) (name=%s) (tag=%d) (#options=%d)\n", node.MapType.KeyType.AsIdentifier(), node.MapType.ValueType.AsIdentifier(), maybe(node.Name).Val, maybe(node.Tag).Val, len(maybe(node.Options).Options)))
	return nil
}

func (v *dumpVisitor) VisitMapTypeNode(node *ast.MapTypeNode) error {
	v.buf.WriteString(fmt.Sprintf("(keytype=%s) (valuetype=%s)\n", maybe(node.KeyType).Val, node.ValueType.AsIdentifier()))
	return nil
}

func (v *dumpVisitor) VisitMessageFieldNode(node *ast.MessageFieldNode) error {
	v.buf.WriteString(fmt.Sprintf("(name=%s) (val=%T)\n", maybe(node.Name).Name.Value(), maybeValue(node.Val)))
	return nil
}

func (v *dumpVisitor) VisitMessageLiteralNode(node *ast.MessageLiteralNode) error {
	v.buf.WriteString(fmt.Sprintf("(#elements=%d)\n", len(node.Elements)))
	return nil
}

func (v *dumpVisitor) VisitMessageNode(node *ast.MessageNode) error {
	v.buf.WriteString(fmt.Sprintf("(name=%s) (#decls=%d)\n", maybe(node.Name).Val, len(node.Decls)))
	return nil
}

func (v *dumpVisitor) VisitNegativeIntLiteralNode(node *ast.NegativeIntLiteralNode) error {
	v.buf.WriteString(fmt.Sprintf("(val=%d)\n", node.Val))
	return nil
}

func (v *dumpVisitor) VisitOneofNode(node *ast.OneofNode) error {
	v.buf.WriteString(fmt.Sprintf("(name=%s) (#decls=%d)\n", maybe(node.Name).Val, len(node.Decls)))
	return nil
}

func (v *dumpVisitor) VisitOptionNameNode(node *ast.OptionNameNode) error {
	v.buf.WriteString(fmt.Sprintf("(name=%s)\n", StringForOptionName(node)))
	return nil
}

func (v *dumpVisitor) VisitOptionNode(node *ast.OptionNode) error {
	if node.IsIncomplete() {
		v.buf.WriteString("(!) ")
	}
	v.buf.WriteString(fmt.Sprintf("(name=%s) (val=%T)\n", StringForOptionName(node.Name), maybeValue(node.Val)))
	return nil
}

func (v *dumpVisitor) VisitPackageNode(node *ast.PackageNode) error {
	v.buf.WriteString(fmt.Sprintf("(name=%s)\n", node.Name))
	return nil
}

func (v *dumpVisitor) VisitPositiveUintLiteralNode(node *ast.PositiveUintLiteralNode) error {
	v.buf.WriteString(fmt.Sprintf("(val=%d)\n", node.Val))
	return nil
}

func (v *dumpVisitor) VisitRPCNode(node *ast.RPCNode) error {
	v.buf.WriteString(fmt.Sprintf("(name=%s) (input=%s) (output=%s) (#decls=%d)\n", maybe(node.Name).Val, maybe(node.Input).MessageType.AsIdentifier(), maybe(node.Output).MessageType.AsIdentifier(), len(node.Decls)))
	return nil
}

func (v *dumpVisitor) VisitRPCTypeNode(node *ast.RPCTypeNode) error {
	v.buf.WriteString(fmt.Sprintf("(name=%s) (stream=%s)\n", node.MessageType.AsIdentifier(), maybe(node.Stream).Val))
	return nil
}

func (v *dumpVisitor) VisitRangeNode(node *ast.RangeNode) error {
	return nil
}

func (v *dumpVisitor) VisitReservedNode(node *ast.ReservedNode) error {
	return nil
}

func (v *dumpVisitor) VisitRuneNode(node *ast.RuneNode) error {
	v.buf.WriteString(fmt.Sprintf("(rune=%s)\n", string(node.Rune)))
	return nil
}

func (v *dumpVisitor) VisitServiceNode(node *ast.ServiceNode) error {
	v.buf.WriteString(fmt.Sprintf("(name=%s) (#decls=%d)\n", maybe(node.Name).Val, len(node.Decls)))
	return nil
}

func (v *dumpVisitor) VisitSignedFloatLiteralNode(node *ast.SignedFloatLiteralNode) error {
	v.buf.WriteString(fmt.Sprintf("(val=%v)\n", node.Val))
	return nil
}

func (v *dumpVisitor) VisitSpecialFloatLiteralNode(node *ast.SpecialFloatLiteralNode) error {
	v.buf.WriteString(fmt.Sprintf("(val=%v)\n", node.Val))
	return nil
}

func (v *dumpVisitor) VisitStringLiteralNode(node *ast.StringLiteralNode) error {
	v.buf.WriteString(fmt.Sprintf("(val=%v)\n", node.Val))
	return nil
}

func (v *dumpVisitor) VisitSyntaxNode(node *ast.SyntaxNode) error {
	v.buf.WriteString(fmt.Sprintf("(syntax=%s)\n", node.Syntax.Value()))
	return nil
}

func (v *dumpVisitor) VisitUintLiteralNode(node *ast.UintLiteralNode) error {
	v.buf.WriteString(fmt.Sprintf("(val=%v)\n", node.Val))
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
