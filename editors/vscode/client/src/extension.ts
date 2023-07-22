import * as vscode from "vscode"
import { LanguageClient } from "vscode-languageclient/node"
import { ASTViewer, fromProtoAstURi } from "./astviewer"
import { buildLanguageClient } from "./client"
import { initCommands } from "./commands"

let client: LanguageClient

export function activate(context: vscode.ExtensionContext) {
  const client = buildLanguageClient(context)
  vscode.workspace.registerTextDocumentContentProvider("proto", client)
  // Start the client. This will also launch the server
  client.start()

  const astViewer = new ASTViewer((uri) => {
    return client.sendRequest("protols/ast", fromProtoAstURi(uri).toString())
  })
  context.subscriptions.push(
    astViewer,
    vscode.workspace.registerTextDocumentContentProvider("protoast", astViewer),
  )

  context.subscriptions.push(
    vscode.commands.registerCommand("protols.restart", async () => {
      try {
        await client.restart()
      } catch (e) {
        // if it's already stopped, restart will throw an error.
        await client.start()
      }
    }),
    vscode.commands.registerCommand("protols.stop", async () => {
      await client.stop()
    }),
    vscode.commands.registerTextEditorCommand("protols.ast", async (editor) => {
      await astViewer.openDocument(editor)
    }),
  )

  initCommands(context)
}

export function deactivate(): Thenable<void> | undefined {
  if (!client) {
    return undefined
  }
  return client.stop()
}
