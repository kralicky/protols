package lsp

import (
	"fmt"

	"github.com/kralicky/protocompile/ast"
	"github.com/kralicky/tools-lite/gopls/pkg/lsp/protocol"
)

func (c *Cache) DocumentSymbolsForFile(doc protocol.TextDocumentIdentifier) ([]protocol.DocumentSymbol, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()

	f, err := c.FindResultByURI(doc.URI)
	if err != nil {
		return nil, err
	}

	var symbols []protocol.DocumentSymbol

	fn := f.AST()
	for _, decl := range fn.Decls {
		switch node := decl.(type) {
		case *ast.MessageNode:
			symbols = append(symbols, messageSymbols(fn, node)...)
		case *ast.EnumNode:
			symbols = append(symbols, enumSymbols(fn, node)...)
		case *ast.ExtendNode:
			symbols = append(symbols, extendSymbols(fn, node)...)
		case *ast.ServiceNode:
			symbols = append(symbols, serviceSymbols(fn, node)...)
		}
	}
	return symbols, nil
}

func messageSymbols(fn *ast.FileNode, node *ast.MessageNode) []protocol.DocumentSymbol {
	symbols := []protocol.DocumentSymbol{}
	sym := protocol.DocumentSymbol{
		Name:           string(node.Name.AsIdentifier()),
		Kind:           protocol.Class,
		Range:          toRange(fn.NodeInfo(node)),
		SelectionRange: toRange(fn.NodeInfo(node.Name)),
	}
	for _, decl := range node.Decls {
		switch node := decl.(type) {
		case *ast.FieldNode:
			if node.IsIncomplete() {
				continue
			}
			sym.Children = append(sym.Children, protocol.DocumentSymbol{
				Name:           string(node.Name.AsIdentifier()),
				Detail:         string(node.FldType.AsIdentifier()),
				Kind:           protocol.Field,
				Range:          toRange(fn.NodeInfo(node)),
				SelectionRange: toRange(fn.NodeInfo(node.Name)),
			})
		case *ast.MapFieldNode:
			sym.Children = append(sym.Children, protocol.DocumentSymbol{
				Name:           string(node.Name.AsIdentifier()),
				Detail:         fmt.Sprintf("map<%s, %s>", string(node.KeyField().Ident.AsIdentifier()), string(node.ValueField().Ident.AsIdentifier())),
				Kind:           protocol.Field,
				Range:          toRange(fn.NodeInfo(node)),
				SelectionRange: toRange(fn.NodeInfo(node.Name)),
			})
		case *ast.MessageNode:
			sym.Children = append(sym.Children, messageSymbols(fn, node)...)
		case *ast.EnumNode:
			sym.Children = append(sym.Children, enumSymbols(fn, node)...)
		case *ast.ExtendNode:
			sym.Children = append(sym.Children, extendSymbols(fn, node)...)
		}
	}
	symbols = append(symbols, sym)
	return symbols
}

func enumSymbols(fn *ast.FileNode, node *ast.EnumNode) []protocol.DocumentSymbol {
	sym := protocol.DocumentSymbol{
		Name:           string(node.Name.AsIdentifier()),
		Kind:           protocol.Enum,
		Range:          toRange(fn.NodeInfo(node)),
		SelectionRange: toRange(fn.NodeInfo(node.Name)),
	}
	for _, decl := range node.Decls {
		switch node := decl.(type) {
		case *ast.EnumValueNode:
			sym.Children = append(sym.Children, protocol.DocumentSymbol{
				Name:           string(node.Name.AsIdentifier()),
				Kind:           protocol.EnumMember,
				Range:          toRange(fn.NodeInfo(node)),
				SelectionRange: toRange(fn.NodeInfo(node.Name)),
			})
		}
	}
	return []protocol.DocumentSymbol{sym}
}

func extendSymbols(fn *ast.FileNode, node *ast.ExtendNode) []protocol.DocumentSymbol {
	symbols := []protocol.DocumentSymbol{}
	for _, decl := range node.Decls {
		switch node := decl.(type) {
		case *ast.FieldNode:
			if node.IsIncomplete() {
				continue
			}
			symbols = append(symbols, protocol.DocumentSymbol{
				Name:           string(node.Name.AsIdentifier()),
				Detail:         fmt.Sprintf("[%s] %s", node.Extendee.Extendee.AsIdentifier(), string(node.FldType.AsIdentifier())),
				Kind:           protocol.Field,
				Range:          toRange(fn.NodeInfo(node)),
				SelectionRange: toRange(fn.NodeInfo(node.Name)),
			})
		}
	}
	return symbols
}

func serviceSymbols(fn *ast.FileNode, node *ast.ServiceNode) []protocol.DocumentSymbol {
	service := protocol.DocumentSymbol{
		Name:           string(node.Name.AsIdentifier()),
		Kind:           protocol.Interface,
		Range:          toRange(fn.NodeInfo(node)),
		SelectionRange: toRange(fn.NodeInfo(node.Name)),
	}
	for _, decl := range node.Decls {
		switch node := decl.(type) {
		case *ast.RPCNode:
			var detail string
			switch {
			case node.Input.Stream != nil && node.Output.Stream != nil:
				detail = "stream (bidirectional)"
			case node.Input.Stream != nil:
				detail = "stream (client)"
			case node.Output.Stream != nil:
				detail = "stream (server)"
			default:
				detail = "unary"
			}
			rpc := protocol.DocumentSymbol{
				Name:           string(node.Name.AsIdentifier()),
				Detail:         detail,
				Kind:           protocol.Method,
				Range:          toRange(fn.NodeInfo(node)),
				SelectionRange: toRange(fn.NodeInfo(node.Name)),
			}
			service.Children = append(service.Children, rpc)
		}
	}
	return []protocol.DocumentSymbol{service}
}
