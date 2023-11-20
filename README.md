# Protobuf Language Server

# Coming Soon™

A language server implementation for Protocol Buffers. Still in development.

Features in progress:
- [x] Code formatting in the style of gofmt
- [x] Import resolution
  - [x] Local/relative paths
  - [x] Go module path lookup with inline sources
  - [x] Go module path lookup with missing proto sources synthesized from generated code
  - [x] Context-sensitive imports and pattern detection
  - [x] Fully interactive sources generated from well-known (or any other) descriptors
- [x] Legacy compatibility
  - [x] gogoproto sources (k8s, etc.)
  - [x] proto2 sources
- [ ] LSP features:
  - [x] Full semantic token support
    - [ ] (partial) Embedded CEL expression semantic tokens
  - [x] Document and workspace diagnostics
  - [x] Import links
  - [x] Find references/definition
    - [x] Types
    - [x] Imports
    - [x] Options, extensions, and field references
    - [x] Inlay Hints
  - [x] Hover
    - [x] Types
    - [x] Options, extensions, and field references
    - [x] Inlay Hints
    - [ ] CEL tokens
  - [ ] Code Actions
    - [x] Identify and remove unused imports
    - [ ] ...
  - [ ] Code Lens
  - [x] Inlay hints
    - [x] Extension types
  - [ ] Rename symbols
  - [x] Multi-workspace support
- [ ] Workspace symbol index/search
  - [ ] Editor Breadcrumbs
- [ ] Code completion:
  - [ ] (partial) Message and enum types
  - [x] Automatic imports
  - [ ] Import paths 
  - [ ] Message and field literals
- [x] Inlay hints for message and field literal types
- [ ] Code generator tools
  - [x] Built-in compiler with workspace context
  - [ ] CLI support
    - [x] 'protols fmt'
    - [x] 'protols vet'
    - [ ] 'protols rename'
    - [ ] ...
  - [ ] Interact with generated code (find references, rename)
- [ ] Debugging tools
  - [x] AST viewer
  - [x] Wire message decoder ('protols decode')
  - [ ] ...
- [ ] Editor support
  - [x] VSCode
  - [ ] Neovim

# Installing

1. Clone this repo
2. Clone submodules: `git submodule update --init`
3. Build and install the protols binary: `go install ./cmd/protols`
4. Install `vsce` if you don't have it: `npm install --global @vscode/vsce`
5. cd to editors/vscode, then run `vsce package`
6. Install the vsix plugin: `code --install-extension ./protols-vscode-<version>.vsix`
   * Note: you may need to change the vscode language association for ".proto" files to `protobuf`, if it is currently set to `proto`.

# Special Thanks

This project is derived from [bufbuild/protocompile](https://github.com/bufbuild/protocompile) and [jhump/protoreflect](https://github.com/jhump/protoreflect). Thanks to the buf developers for their fantastic work.

Many of the components of [gopls](https://github.com/golang/tools/tree/master/gopls) are used to build the language server. A fork of golang/tools is maintained [here](https://github.com/kralicky/tools).
