import * as vscode from "vscode"

type ASTFetcher = (
  uri: vscode.Uri,
  version: number,
  token: vscode.CancellationToken,
) => Promise<string>

export class ASTViewer implements vscode.TextDocumentContentProvider {
  private virtualUrisByFile = new Map<string, vscode.Uri>()
  private documentVersionsByUri = new Map<string, number>()
  private closeListener: vscode.Disposable
  private changeListener: vscode.Disposable
  private fetchAST: ASTFetcher

  constructor(fetchAST: ASTFetcher) {
    this.fetchAST = fetchAST
    this.closeListener = vscode.workspace.onDidCloseTextDocument((doc) => {
      switch (doc.uri.scheme) {
        case "file": {
          this.virtualUrisByFile.delete(doc.uri.toString())
          this.documentVersionsByUri.delete(doc.uri.toString())
          break
        }
        case "protoast2":
        case "protoast": {
          const uri = fromProtoAstUri(doc.uri)
          this.virtualUrisByFile.delete(uri.toString())
          this.documentVersionsByUri.delete(uri.toString())
          break
        }
      }
    })
    this.changeListener = vscode.workspace.onDidChangeTextDocument((e) => {
      const virtualUri = this.virtualUrisByFile.get(e.document.uri.toString())
      if (virtualUri) {
        this.documentVersionsByUri.set(
          virtualUri.toString(),
          e.document.version,
        )
        this.refresh(virtualUri)
      }
    })
  }
  private _onDidChange = new vscode.EventEmitter<vscode.Uri>()

  get onDidChange(): vscode.Event<vscode.Uri> {
    return this._onDidChange.event
  }

  async openDocument(editor: vscode.TextEditor): Promise<vscode.TextEditor> {
    if (editor.document.languageId !== "protobuf") {
      vscode.window.showErrorMessage("Cannot show AST for non-protobuf file")
      return
    }
    const virtualUri = toProtoAstUri(editor.document.uri)
    this.virtualUrisByFile.set(editor.document.uri.toString(), virtualUri)

    const virtualDoc = await vscode.workspace.openTextDocument(virtualUri)

    await vscode.window.showTextDocument(virtualDoc, {
      preview: false,
      viewColumn: vscode.ViewColumn.Beside,
      preserveFocus: true,
    })
  }

  refresh(uri: vscode.Uri) {
    this._onDidChange.fire(uri)
  }

  provideTextDocumentContent(
    uri: vscode.Uri,
    token: vscode.CancellationToken,
  ): vscode.ProviderResult<string> {
    const fileUri = fromProtoAstUri(uri)
    if (!this.virtualUrisByFile.has(fileUri.toString())) {
      return ""
    }
    const version = this.documentVersionsByUri.get(uri.toString()) ?? 0
    return this.fetchAST(fileUri, version, token)
  }

  public dispose() {
    this.closeListener.dispose()
    this.changeListener.dispose()
  }
}

export function toProtoAstUri(uri: vscode.Uri): vscode.Uri {
  if (uri.scheme === "file") {
    return uri.with({
      scheme: "protoast",
      path: uri.path + " [AST]",
    })
  } else if (uri.scheme === "proto") {
    return uri.with({
      scheme: "protoast2",
      path: uri.path + " [AST]",
    })
  } else {
    return uri
  }
}

export function fromProtoAstUri(uri: vscode.Uri): vscode.Uri {
  if (uri.scheme === "protoast") {
    return uri.with({
      scheme: "file",
      path: uri.path.replace(/ \[AST\]$/, ""),
    })
  } else if (uri.scheme === "protoast2") {
    return uri.with({
      scheme: "proto",
      path: uri.path.replace(/ \[AST\]$/, ""),
    })
  } else {
    return uri
  }
}
