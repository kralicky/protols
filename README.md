# Protobuf Language Server

# Coming Soon™

A language server implementation for Protocol Buffers. Still in development.

Planned Features:
- Semantic token syntax highlighting
- Code formatting in the style of gofmt
- Detailed workspace diagnostics
- Several different import resolution strategies including importing from Go modules
- Go-to-definition, find references, and hover support
- Code completion for messages, enums, and import paths (with automatic import management)
- Inlay hints for message and field literal types
- Code generator tools built in
- VSCode and Neovim support

Built using modified versions of bufbuild/protocompile, jhump/protoreflect, and golang/tools.