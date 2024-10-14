package format

import (
	"bytes"
	"fmt"
	"io"

	"github.com/kralicky/protocompile/ast"
)

type elemKind int

const (
	nonOptionKind elemKind = iota + 1
	optionKind
)

func groupableNodeType(t ast.Node) (elemKind, bool) {
	switch t.(type) {
	case *ast.FieldNode, *ast.MapFieldNode, *ast.EnumValueNode, *ast.MessageFieldNode:
		return nonOptionKind, true
	case *ast.OptionNode:
		return optionKind, true
	}
	return 0, false
}

type elementsContainer[T ast.Node] interface {
	GetElements() []T
}

type segmentedField struct {
	contextBytesStart []byte
	typeName          []byte
	fieldName         []byte
	equalsTag         []byte
	lineEnd           []byte
}

func columnFormatElements[T ast.Node, C elementsContainer[T]](f *formatter, ctr C) {
	elems := ctr.GetElements()
	groups := [][]ast.Node{}
	currentGroup := []ast.Node{}
	startNewGroup := func() {
		if len(currentGroup) > 0 {
			groups = append(groups, currentGroup)
		}
		currentGroup = []ast.Node{}
	}
	var kind elemKind
	var isGroupable bool
	for i := 0; i < len(elems); i++ {
		e := ast.Unwrap(elems[i])
		if i > 0 {
			k, g := groupableNodeType(e)
			if k != kind || g != isGroupable {
				startNewGroup()
			}
			kind = k
			isGroupable = g
		} else {
			kind, isGroupable = groupableNodeType(e)
		}
		if isGroupable {
			fieldInfo := f.fileNode.NodeInfo(e)
			if len(currentGroup) == 0 {
				currentGroup = append(currentGroup, e)
				continue
			}

			// group this field with the previous field if they are on directly
			// consecutive lines
			switch fieldNode := e.(type) {
			case *ast.FieldNode:
				if fieldNode.IsIncomplete() {
					// don't group incomplete fields
					startNewGroup()
					currentGroup = append(currentGroup, e)
					continue
				}
				// check if we are about to expand a compact options group
				if fieldNode.Options != nil && f.compactOptionsShouldBeExpanded(fieldNode.Options) {
					startNewGroup()
					currentGroup = append(currentGroup, e)
					startNewGroup() // group this field by itself
					continue
				}
			case *ast.EnumValueNode:
				// check if we are about to expand a compact options group
				if fieldNode.Options != nil && f.compactOptionsShouldBeExpanded(fieldNode.Options) {
					startNewGroup()
					currentGroup = append(currentGroup, e)
					startNewGroup() // group this field by itself
					continue
				}
			case *ast.MessageFieldNode:
				// break groups on multiline message/array literals.
				// this prevents code that looks like:
				// option ... {
				//   foo:               a
				//   bar:               b
				//   some_other_message {
				//     ...
				//   }
				// }
				isMessageOrArrayLiteral := false
				switch val := fieldNode.Val.Unwrap().(type) {
				case *ast.MessageLiteralNode:
					if f.messageLiteralShouldBeExpanded(val) {
						isMessageOrArrayLiteral = true
					}
				case *ast.ArrayLiteralNode:
					if f.arrayLiteralShouldBeExpanded(val) {
						isMessageOrArrayLiteral = true
					}
				}
				if isMessageOrArrayLiteral {
					startNewGroup()
					currentGroup = append(currentGroup, e)
					startNewGroup() // group this field by itself
					continue
				}
			}
			prevFieldInfo := f.fileNode.NodeInfo(currentGroup[len(currentGroup)-1])
			if line, prevLine := fieldInfo.Start().Line, prevFieldInfo.Start().Line; line == prevLine || line == prevLine+1 {
				// don't group if there are comments between the fields that would cause
				// them to be separated
				if fieldInfo.LeadingComments().Len() > 0 {
					startNewGroup()
				}
				currentGroup = append(currentGroup, e)
				continue
			} else {
				// the field is not directly adjacent to the previous field, but that
				// doesn't necessarily mean we should start a new group. There are a few
				// edge cases to consider:

				// 1. The previous field was formatted multiline, but we compacted it:
				//    int32 foo = 1 [
				//      first =
				//        foo,
				//      second = bar
				//    ];
				// 2. A type name is split up between dots:
				//    long.package
				//      .name.Foo name = 1;
				shouldStartNewGroup := true
				if line == prevFieldInfo.End().Line+1 {
					switch prevNode := ast.Node(currentGroup[len(currentGroup)-1]).(type) {
					case *ast.OptionNode, *ast.MessageFieldNode:
						var prevVal ast.Node
						switch prevNode := prevNode.(type) {
						case *ast.OptionNode:
							prevVal = prevNode.Val.Unwrap()
						case *ast.MessageFieldNode:
							prevVal = prevNode.Val.Unwrap()
						}
						switch prevVal := prevVal.(type) {
						case *ast.ArrayLiteralNode:
							if !f.arrayLiteralShouldBeExpanded(prevVal) {
								shouldStartNewGroup = false
							}
						case *ast.MessageLiteralNode:
							if !f.messageLiteralShouldBeExpanded(prevVal) {
								shouldStartNewGroup = false
							}
						case *ast.CompoundStringLiteralNode:
						default:
							shouldStartNewGroup = false
						}
					case *ast.FieldNode:
						if prevFieldTypeInfo := f.fileNode.NodeInfo(prevNode.FieldType.Unwrap()); prevFieldTypeInfo.IsValid() {
							if prevFieldTypeInfo.End().Line > prevFieldTypeInfo.Start().Line {
								shouldStartNewGroup = false
							}
						}
						if prevNode.Options != nil && !f.compactOptionsShouldBeExpanded(prevNode.Options) {
							shouldStartNewGroup = false
						}
					}
				}

				if shouldStartNewGroup {
					startNewGroup()
				}
				currentGroup = append(currentGroup, e)
			}
		} else {
			startNewGroup()
			currentGroup = append(currentGroup, e)
			startNewGroup()
		}
	}
	startNewGroup()

	groupStartIndexes := make([]int, len(groups))
	for i := range groups {
		if i == 0 {
			groupStartIndexes[i] = 0
			continue
		}
		groupStartIndexes[i] = groupStartIndexes[i-1] + len(groups[i-1])
	}
GROUPS:
	for groupIdx, groupElem := range groups {
		bufferedFields := []segmentedField{}
		colBuf := new(bytes.Buffer)
		fclone := f.saveState(colBuf)
		startIndex := groupStartIndexes[groupIdx]
		for i, elem := range groupElem {
			elemIdx := startIndex + i
			field := segmentedField{}
			nodeWriter := func(n ast.Node) {
				field.contextBytesStart, _ = io.ReadAll(colBuf)
				fclone.writeNode(n)
			}
			switch elem := elem.(type) {
			case *ast.FieldNode:
				if elem.IsIncomplete() {
					fclone.writeField(elem)
					continue
				}
				if elem.Label != nil {
					fclone.writeStart(elem.Label, nodeWriter)
					fclone.Space()
					fclone.writeInline(elem.FieldType.Unwrap())
				} else {
					// If a label was not written, the multiline comments will be
					// attached to the type.
					if compoundIdentNode := elem.FieldType.GetCompoundIdent(); compoundIdentNode != nil {
						fclone.writeCompountIdentForFieldName(compoundIdentNode, nodeWriter)
					} else {
						fclone.writeStart(elem.FieldType.GetIdent(), nodeWriter)
					}
				}
				// flush the buffer to save the type name
				field.typeName, _ = io.ReadAll(colBuf)

				fclone.writeInline(elem.Name)
				// flush the buffer to save the field name
				field.fieldName, _ = io.ReadAll(colBuf)

				fclone.writeInline(elem.Equals)
				fclone.Space()
				fclone.writeInline(elem.Tag)
				// flush the buffer to save the equals byte and tag
				field.equalsTag, _ = io.ReadAll(colBuf)

				if elem.Options != nil {
					fclone.writeNode(elem.Options)
				}
				fclone.writeLineEnd(elem.Semicolon)
				// flush the buffer to save the options and semicolon
				field.lineEnd, _ = io.ReadAll(colBuf)
			case *ast.MapFieldNode:
				fclone.writeStart(elem.MapType.Keyword, nodeWriter)
				fclone.writeInline(elem.MapType.OpenAngle)
				fclone.writeInline(elem.MapType.KeyType)
				fclone.writeInline(elem.MapType.Comma)
				fclone.Space()
				fclone.writeInline(elem.MapType.ValueType)
				fclone.writeInline(elem.MapType.CloseAngle)
				if vs := elem.MapType.Semicolon; vs != nil {
					info := f.fileNode.NodeInfo(vs)
					if info.TrailingComments().Len() > 0 {
						f.writeInlineComments(info.TrailingComments())
					}
				}
				// flush the buffer to save the type name
				field.typeName, _ = io.ReadAll(colBuf)

				fclone.writeInline(elem.Name)
				// flush the buffer to save the field name
				field.fieldName, _ = io.ReadAll(colBuf)

				fclone.writeInline(elem.Equals)
				fclone.Space()
				fclone.writeInline(elem.Tag)
				// flush the buffer to save the equals byte and tag
				field.equalsTag, _ = io.ReadAll(colBuf)

				if elem.Options != nil {
					fclone.writeNode(elem.Options)
				}
				fclone.writeLineEnd(elem.Semicolon)
				// flush the buffer to save the options and semicolon
				field.lineEnd, _ = io.ReadAll(colBuf)
			case *ast.EnumValueNode:
				fclone.writeStart(elem.Name, nodeWriter)
				// flush the buffer to save the field name
				field.fieldName, _ = io.ReadAll(colBuf)

				fclone.writeInline(elem.Equals)
				fclone.Space()
				fclone.writeInline(elem.Number)
				// flush the buffer to save the equals byte and tag
				field.equalsTag, _ = io.ReadAll(colBuf)

				if elem.Options != nil {
					fclone.writeNode(elem.Options)
				}
				if elem.Semicolon.Rune != ';' {
					// fix extended syntax
					elem.Semicolon.Rune = ';'
				}
				fclone.writeLineEnd(elem.Semicolon)
				// flush the buffer to save the options and semicolon
				field.lineEnd, _ = io.ReadAll(colBuf)
			case *ast.MessageFieldNode:
				if elem.Name.Open != nil {
					fclone.writeStart(elem.Name.Open, nodeWriter)
					if elem.Name.UrlPrefix != nil {
						fclone.writeInline(elem.Name.UrlPrefix)
					}
					if elem.Name.Slash != nil {
						fclone.writeInline(elem.Name.Slash)
					}
					fclone.writeInline(elem.Name.Name)
				} else {
					fclone.writeStart(elem.Name.Name, nodeWriter)
				}
				if elem.Name.Close != nil {
					fclone.writeInline(elem.Name.Close)
				} else if elem.Name.Open != nil {
					// (extended syntax rule) fill in missing close paren automatically
					fclone.writeInline(&ast.RuneNode{Rune: ')'})
				}
				if elem.Sep != nil {
					fclone.writeInline(elem.Sep)
				} else {
					// fill in missing ':' automatically
					fclone.writeInline(&ast.RuneNode{Rune: ':'})
				}
				// flush the buffer to save the field name
				field.fieldName, _ = io.ReadAll(colBuf)

				if elem.Semicolon != nil {
					if elem.Semicolon.Rune != ',' {
						elem.Semicolon.Rune = ','
					}
				} else {
					elem.Semicolon = &ast.RuneNode{Rune: ',', Token: elem.End()}
				}
				fclone.writeNode(elem.Val)
				fclone.writeLineEnd(elem.Semicolon)

				field.lineEnd, _ = io.ReadAll(colBuf)
			case *ast.OptionNode:
				// column format for options that look like:
				// option go_package = "...";
				// option java_multiple_files = true;
				// option java_outer_classname = "...";
				// option java_package = "...";

				// to align the equals signs:
				// option go_package           = "...";
				// option java_multiple_files  = true;
				// option java_outer_classname = "...";
				// option java_package         = "...";

				// also handle the following:
				// optional int foo = 1 [
				//   (testing) = 1,
				//   (test)    = 2
				// ]

				// Write the 'option' keyword
				if elem.Keyword != nil {
					fclone.writeStartMaybeCompact(elem.Keyword, false, nodeWriter)
					field.typeName, _ = io.ReadAll(colBuf)
				}

				// write the option name
				if elem.Name != nil {
					if elem.Keyword == nil {
						fclone.writeStartMaybeCompact(elem.Name, false, nodeWriter)
					} else {
						fclone.writeNode(elem.Name)
					}
					field.fieldName, _ = io.ReadAll(colBuf)
				}

				// Write the equals sign
				if elem.Equals != nil {
					fclone.writeInline(elem.Equals)
					field.equalsTag, _ = io.ReadAll(colBuf)
				}

				// Write the option value
				if node := elem.Val.GetCompoundStringLiteral(); node != nil {
					// Compound string literals are written across multiple lines
					// immediately after the '=', so we don't need a trailing
					// space in the option prefix.
					fclone.writeCompoundStringLiteralIndentEndInline(node)
					fclone.writeLineEnd(elem.Semicolon)
					field.lineEnd, _ = io.ReadAll(colBuf)
				} else {
					if f.inCompactOptions {
						compactOptionsNode := any(ctr).(*ast.CompactOptionsNode)
						if elemIdx == len(elems)-1 {
							if elem.Val != nil {
								fclone.writeLineEnd(elem.Val)
							}
						} else {
							if elem.Val != nil {
								fclone.writeNode(elem.Val)
							}
							semi := compactOptionsNode.Options[elemIdx].Semicolon
							if semi.Rune != ',' {
								semi.Rune = ','
							}
							fclone.writeLineEnd(semi)
						}
					} else {
						if elem.Val != nil {
							fclone.writeNode(elem.Val)
						}
						fclone.writeLineEnd(elem.Semicolon)
					}
					field.lineEnd, _ = io.ReadAll(colBuf)
				}
			default:
				if len(groupElem) == 1 {
					// still need to handle the node-specific formatting logic above,
					// since there could be some non-groupable nodes mixed in, each
					// of which will be in its own group.
					f.writeNode(elem)
					continue GROUPS
				}
				panic(fmt.Sprintf("column formatting not implemented for element type %T", elem))
			}
			bufferedFields = append(bufferedFields, field)
		}

		for block := range splitSegmentedFields(bufferedFields) {
			// find the longest string in each column
			typeNameCol, fieldNameCol, equalsTagCol, optionsSemicolonCol := 0, 0, 0, 0
			for i, field := range block {
				typeNameCol = max(typeNameCol, len(field.typeName))
				fieldNameCol = max(fieldNameCol, len(field.fieldName))
				equalsTagCol = max(equalsTagCol, len(field.equalsTag))
				if len(field.lineEnd) > 0 && field.lineEnd[0] == ' ' {
					// TODO: inline comments between a field's tag and option start bracket
					// (i.e. `int foo = 1 /* comment */ [...];`)
					// can result in an extra leading space in field.lineEnd. This appears
					// to be caused by writeInlineComments called from writeOpenBracePrefix.
					field.lineEnd = field.lineEnd[1:]
					block[i] = field
				}
				optionsSemicolonCol = max(optionsSemicolonCol, len(field.lineEnd))
			}

			for _, field := range block {
				colBuf.Write(field.contextBytesStart)
				colBuf.Write(field.typeName)
				typeNamePadding, fieldNamePadding, equalsTagPadding := 1, 1, 1
				if len(field.typeName) == 0 {
					typeNamePadding = 0
				}
				if len(field.fieldName) == 0 {
					fieldNamePadding = 0
				}
				if len(field.equalsTag) == 0 {
					equalsTagPadding = 0
				}
				colBuf.Write(bytes.Repeat([]byte{' '}, typeNameCol-len(field.typeName)+typeNamePadding))
				colBuf.Write(field.fieldName)
				colBuf.Write(bytes.Repeat([]byte{' '}, fieldNameCol-len(field.fieldName)+fieldNamePadding))
				colBuf.Write(field.equalsTag)
				if len(field.lineEnd) > 0 && field.lineEnd[0] == ';' {
					// don't write spaces before the semicolon
					colBuf.Write(field.lineEnd)
				} else {
					colBuf.Write(bytes.Repeat([]byte{' '}, equalsTagCol-len(field.equalsTag)+equalsTagPadding))
					colBuf.Write(field.lineEnd)
				}
			}
		}
		f.mergeState(fclone, colBuf)
	}
}
