<img src="https://raw.githubusercontent.com/kralicky/protols/main/editors/vscode/logo.png" width="96" align="left"/> <h1 align="left"><p>Protobuf Language Server</p></h1>

![Release](https://img.shields.io/github/v/release/kralicky/protols)
[![CI](https://github.com/kralicky/protols/actions/workflows/ci.yaml/badge.svg)](https://github.com/kralicky/protols/actions/workflows/test.yaml)
[![License](https://img.shields.io/github/license/kralicky/protols)](./LICENSE)

A language server implementation for Protocol Buffers. Under active development; some features are still a work in progress.

### LSP features:

- [x] Document Formatting
- [x] Full semantic token support
  - [ ] (partial) Embedded CEL expression semantic tokens
- [x] Document and workspace diagnostics
- [x] Import links
- [x] Find references/definition
  - [x] Types and enums
  - [x] Imports
  - [x] Options, extensions, and field references
  - [x] Inlay Hints
  - [x] Package names and prefixes
- [x] Hover
  - [x] Types and enums
  - [x] Options, extensions, and field references
  - [x] Inlay Hints
  - [x] Package names and prefixes
  - [ ] CEL tokens
- [ ] Code Actions & Refactors
  - [x] Identify and remove unused imports
  - [x] Add missing import for unresolved symbol
  - [x] Auto-fix imports on save
  - [x] Simplify repeated option declarations
  - [x] Simplify repeated message literal fields
  - [ ] Simplify map literal fields
  - [x] Extract fields to new message
  - [x] Inline fields from message
  - [x] Renumber message fields
- [x] Code Lens
  - [x] Generate file/package/workspace
- [x] Inlay hints
  - [x] Extension types
  - [x] Resolved import paths
- [x] Rename symbols
- [x] Multi-workspace support
- [x] Document symbols
- [x] Workspace symbol query with fuzzy matching
- [ ] Completion:
  - [x] Message and enum types
  - [x] Extendee types
  - [x] Context-sensitive keywords
  - [x] Import paths
  - [x] Package names
  - [x] Message and field literals
  - [ ] Field literal values
- [x] Import resolution
  - [x] Local/relative paths
  - [x] Go module path lookup with inline sources
  - [x] Go module path lookup with missing proto sources synthesized from generated code
  - [x] Context-sensitive imports and pattern detection
  - [x] Import path lookup from existing generated Go code
  - [x] Fully interactive sources generated from well-known (or any other) descriptors
- [x] Legacy compatibility
  - [x] gogoproto sources (k8s, etc.)
  - [x] proto2 sources
- [ ] Future compatibility
  - [ ] Editions
- [ ] Code generator tools
  - [x] Built-in compiler with workspace context
  - [ ] CLI support
    - [x] 'protols fmt'
    - [x] 'protols vet'
    - [ ] 'protols rename'
    - [ ] ...
  - [ ] Interact with generated code
    - [x] Go to Generated Definition
    - [ ] Find references
    - [ ] Call hierarchy
    - [ ] Cross-language rename
- [ ] Debugging tools
  - [x] AST viewer
  - [x] Wire message decoder ('protols decode')
  - [ ] ...
- [ ] Editor support
  - [x] VSCode
  - [ ] Neovim

# Installing

1. Clone this repo
2. Build and install the protols binary: `go install ./cmd/protols`
3. Install `vsce` if you don't have it: `npm install --global @vscode/vsce`
4. cd to editors/vscode, then run `vsce package`
5. Install the vsix plugin: `code --install-extension ./protols-vscode-<version>.vsix`

# Special Thanks

This project is derived from [bufbuild/protocompile](https://github.com/bufbuild/protocompile) and [jhump/protoreflect](https://github.com/jhump/protoreflect). Thanks to the buf developers for their fantastic work.

Several packages in https://github.com/golang/tools are used to build the language server. A minimal subset of its lsp-related packages are maintained as a library at https://github.com/kralicky/tools-lite.
