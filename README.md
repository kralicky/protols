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
  - [x] Multi-workspace support
- Code completion:
  - [x] Message and enum types (with automatic import management)
  - [ ] Import paths 
  - [ ] Message and field literals
- [x] Inlay hints for message and field literal types
- [ ] Code generator tools built in
- [ ] Editor support
  - [x] VSCode
  - [ ] Neovim

# Installing

1. Clone this repo
2. `go install ./protols`
3. Install `vsce` if you don't have it: `npm install --global @vscode/vsce`
4. cd to editors/vscode, then run `vsce package`
5. Install the vsix plugin

# Special Thanks

This project is derived from [bufbuild/protocompile](https://github.com/bufbuild/protocompile) and [jhump/protoreflect](https://github.com/jhump/protoreflect). Thanks to the buf developers for their fantastic work.

Many of the components of [gopls](https://github.com/golang/tools/tree/master/gopls) are used to build the language server. A fork of golang/tools is maintained [here](https://github.com/kralicky/tools).
