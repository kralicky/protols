package lsp

import (
	"context"
	"fmt"
	"runtime"

	"github.com/kralicky/protocompile/ast"
	"github.com/kralicky/protocompile/parser"
	"github.com/kralicky/protocompile/protoutil"
	"github.com/kralicky/protols/pkg/x/symbols"
	"github.com/kralicky/tools-lite/gopls/pkg/lsp/protocol"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func (c *Cache) DocumentSymbolsForFile(uri protocol.DocumentURI) ([]protocol.DocumentSymbol, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	return c.documentSymbolsForFileLocked(uri)
}

func (c *Cache) documentSymbolsForFileLocked(uri protocol.DocumentURI) ([]protocol.DocumentSymbol, error) {
	f, err := c.FindResultByURI(uri)
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

func (c *Cache) QueryWorkspaceSymbols(ctx context.Context, query string) []protocol.SymbolInformation {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()

	descriptors := make(chan protoreflect.Descriptor, runtime.NumCPU()+1)

	store := symbols.NewSymbolStore(func(si symbols.SymbolInformation[protoreflect.Descriptor]) (protocol.SymbolInformation, bool) {
		parent := si.Data.ParentFile()
		uri, err := c.resolver.PathToURI(parent.Path())
		if err != nil {
			return protocol.SymbolInformation{}, false
		}
		res, err := c.FindResultByURI(uri)
		if err != nil {
			return protocol.SymbolInformation{}, false
		}
		return toSymbolInformation(uri, res, si.Data)
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		matchFunc := symbols.ParseQuery(query, symbols.NewFuzzyMatcher)
		for desc := range descriptors {
			_, score := matchFunc([]string{string(desc.FullName())})
			store.Store(symbols.SymbolInformation[protoreflect.Descriptor]{
				Score:  float64(score),
				Symbol: string(desc.FullName()),
				Data:   desc,
			})
		}
	}()
	err := c.rangeAllDescriptorsLocked(ctx, func(d protoreflect.Descriptor) bool {
		switch d := d.(type) {
		case protoreflect.MessageDescriptor:
			if d.IsPlaceholder() || d.IsMapEntry() {
				return true
			}
		case protoreflect.EnumDescriptor:
		case protoreflect.ServiceDescriptor:
		case protoreflect.MethodDescriptor:
		case protoreflect.FieldDescriptor:
		case protoreflect.EnumValueDescriptor:
		default:
			return true
		}

		descriptors <- d
		return true
	})
	close(descriptors)
	if err != nil {
		return nil
	}
	select {
	case <-done:
		return store.Results()
	case <-ctx.Done():
		return nil
	}
}

func toSymbolInformation(uri protocol.DocumentURI, res parser.Result, desc protoreflect.Descriptor) (protocol.SymbolInformation, bool) {
	if wrapper, ok := desc.(protoutil.DescriptorProtoWrapper); ok {
		descpb := wrapper.AsProto()
		fileNode := res.AST()
		if node := res.Node(descpb); node != nil {
			info := fileNode.NodeInfo(node)
			var containerName string
			if _, ok := desc.Parent().(protoreflect.FileDescriptor); !ok {
				containerName = string(desc.Parent().FullName())
			}
			return protocol.SymbolInformation{
				Name: string(desc.FullName()),
				Kind: symbolKind(desc),
				Location: protocol.Location{
					URI:   uri,
					Range: toRange(info),
				},
				ContainerName: containerName,
			}, true
		}
	}
	return protocol.SymbolInformation{}, false
}

func symbolKind(desc protoreflect.Descriptor) protocol.SymbolKind {
	switch desc.(type) {
	case protoreflect.MessageDescriptor:
		return protocol.Class
	case protoreflect.EnumDescriptor:
		return protocol.Enum
	case protoreflect.ServiceDescriptor:
		return protocol.Interface
	case protoreflect.MethodDescriptor:
		return protocol.Function
	case protoreflect.FieldDescriptor:
		return protocol.Field
	case protoreflect.EnumValueDescriptor:
		return protocol.EnumMember
	default:
		return protocol.Null
	}
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
