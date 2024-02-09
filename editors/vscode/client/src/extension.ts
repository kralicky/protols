import * as vscode from "vscode"
import { LanguageClient } from "vscode-languageclient/node"
import { ASTViewer, fromProtoAstUri } from "./astviewer"
import { buildLanguageClient } from "./client"
import { initCommands } from "./commands"
import { Location } from "vscode-languageserver-types"

let client: LanguageClient

export async function activate(context: vscode.ExtensionContext) {
  const client = await buildLanguageClient(context)
  vscode.workspace.registerTextDocumentContentProvider("proto", client)
  // Start the client. This will also launch the server
  client.start()

  const astViewer = new ASTViewer(
    (uri: vscode.Uri, version: number, token: vscode.CancellationToken) => {
      return client
        .sendRequest(
          "workspace/executeCommand",
          {
            command: "protols/ast",
            arguments: [
              {
                uri: fromProtoAstUri(uri).toString(),
                version,
              },
            ],
          },
          token,
        )
        .then((result: string) => {
          return result
        })
    },
  )
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
    vscode.commands.registerCommand("protols.reindexWorkspaces", async () => {
      if (!client.isRunning()) {
        return
      }
      await client.sendRequest("workspace/executeCommand", {
        command: "protols/reindexWorkspaces",
        arguments: [],
      })
    }),
    vscode.commands.registerCommand("protols.refreshModules", async () => {
      if (!client.isRunning()) {
        return
      }
      await client.sendRequest("workspace/executeCommand", {
        command: "protols/refreshModules",
        arguments: [],
      })
    }),
    vscode.commands.registerCommand("protols.stop", async () => {
      if (!client.isRunning()) {
        return
      }
      await client.stop()
    }),
    vscode.commands.registerTextEditorCommand(
      "protols.goToGeneratedDefinition",
      async (editor) => {
        if (!client.isRunning()) {
          return
        }
        try {
          await client
            .sendRequest("workspace/executeCommand", {
              command: "protols/goToGeneratedDefinition",
              arguments: [
                client.code2ProtocolConverter.asTextDocumentPositionParams(
                  editor.document,
                  editor.selection.anchor,
                ),
              ],
            })
            .then((result: Location[]) => {
              const locations = result.map(
                client.protocol2CodeConverter.asLocation,
              )
              if (locations.length) {
                if (locations.length === 1) {
                  vscode.commands.executeCommand(
                    "vscode.open",
                    locations[0].uri.with({
                      fragment: `L${locations[0].range.start.line + 1},${
                        locations[0].range.start.character + 1
                      }`,
                    }),
                  )
                } else {
                  vscode.commands.executeCommand(
                    "editor.action.showReferences",
                    editor.document.uri,
                    editor.selection.anchor,
                    locations,
                  )
                }
                return
              }
            })
        } catch (e) {
          vscode.window.showErrorMessage(e.message)
        }
      },
    ),
    vscode.commands.registerTextEditorCommand(
      "protols.generateWorkspace",
      async (editor) => {
        if (!client.isRunning()) {
          return
        }
        const workspaceForEditor = vscode.workspace.getWorkspaceFolder(
          editor.document.uri,
        )
        try {
          await client.sendRequest("workspace/executeCommand", {
            command: "protols/generateWorkspace",
            arguments: [
              {
                workspace: {
                  uri: client.code2ProtocolConverter.asUri(
                    workspaceForEditor.uri,
                  ),
                  name: workspaceForEditor.name,
                },
              },
            ],
          })
        } catch (e) {
          vscode.window.showErrorMessage(e.message)
        }
      },
    ),
    vscode.commands.registerTextEditorCommand(
      "protols.generate",
      async (editor, _, ...args) => {
        if (!client.isRunning()) {
          return
        }
        try {
          await client.sendRequest("workspace/executeCommand", {
            command: "protols/generate",
            arguments: args || [
              {
                uris: [
                  client.code2ProtocolConverter.asUri(editor.document.uri),
                ],
              },
            ],
          })
        } catch (e) {
          vscode.window.showErrorMessage(e.message)
        }
      },
    ),
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
