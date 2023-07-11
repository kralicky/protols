# Protobuf Language Server

# Coming Soon™

A language server implementation for Protocol Buffers. Still in development.

Features in progress:
- [x] Semantic token syntax highlighting
- [x] Code formatting in the style of gofmt
- [x] Import resolution
  - [x] Filesystem
  - [x] Go module path lookup with inline sources
  - [x] Go module path lookup with missing proto sources synthesized from generated code
- [ ] LSP features:
  - [x] Document and workspace diagnostics
  - [x] Import links
  - [x] Go-to-definition
  - [x] Find references
  - [ ] Find references to generated code
  - [x] Hover
  - [ ] Document and workspace symbols
  - [ ] Code Actions
  - [ ] Code Lens
  - [ ] Rename symbols
- Code completion:
  - [x] Message and enum types (with automatic import management)
  - [ ] Import paths 
  - [ ] Message and field literals
- [x] Inlay hints for message and field literal types
- [ ] Code generator tools built in
- [ ] Editor support
  - [x] VSCode
  - [ ] Neovim

Built using modified versions of bufbuild/protocompile, jhump/protoreflect, and golang/tools.
