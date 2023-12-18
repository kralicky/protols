import * as vscode from "vscode"
import { LanguageClient } from "vscode-languageclient/node"
import { ASTViewer, fromProtoAstUri } from "./astviewer"
import { buildLanguageClient } from "./client"
import { initCommands } from "./commands"

let client: LanguageClient

export async function activate(context: vscode.ExtensionContext) {
  const client = await buildLanguageClient(context)
  vscode.workspace.registerTextDocumentContentProvider("proto", client)
  // Start the client. This will also launch the server
  client.start()

  const astViewer = new ASTViewer((uri) => {
    return client
      .sendRequest("workspace/executeCommand", {
        command: "protols/ast",
        arguments: [{ uri: fromProtoAstUri(uri).toString() }],
      })
      .then((result: string) => {
        return result
      })
  })
  context.subscriptions.push(
    astViewer,
    vscode.workspace.registerTextDocumentContentProvider("protoast", astViewer),
  )
  context.subscriptions.push(
    astViewer,
    vscode.workspace.registerTextDocumentContentProvider(
      "protoast2",
      astViewer,
    ),
  )

  context.subscriptions.push(
    vscode.commands.registerCommand("protols.restart", async () => {
      if (!client.isRunning()) {
        await client.start()
      } else {
        await client.restart()
      }
    }),
    vscode.commands.registerCommand("protols.reindex-workspaces", async () => {
      if (!client.isRunning()) {
        return
      }
      await client.sendRequest("workspace/executeCommand", {
        command: "protols/reindex-workspaces",
        arguments: [],
      })
    }),
    vscode.commands.registerCommand("protols.stop", async () => {
      if (!client.isRunning()) {
        return
      }
      await client.stop()
    }),
    vscode.commands.registerTextEditorCommand("protols.ast", async (editor) => {
      if (!client.isRunning()) {
        return
      }
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
