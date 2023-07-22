import * as vscode from "vscode"
import {
  LanguageClientOptions,
  RevealOutputChannelOn,
} from "vscode-languageclient"
import {
  LanguageClient,
  ServerOptions,
  TransportKind,
} from "vscode-languageclient/node"
import * as path from "path"

export class ProtolsLanguageClient
  extends LanguageClient
  implements vscode.TextDocumentContentProvider
{
  constructor(
    id: string,
    name: string,
    serverOptions: ServerOptions,
    clientOptions: LanguageClientOptions,
  ) {
    super(id, name, serverOptions, clientOptions)
  }
  onDidChange?: vscode.Event<vscode.Uri>
  provideTextDocumentContent(
    uri: vscode.Uri,
    token: vscode.CancellationToken,
  ): vscode.ProviderResult<string> {
    return this.sendRequest(
      "protols/synthetic-file-contents",
      uri.toString(),
    ).then((result: string) => {
      return result
    })
  }
}

export function buildLanguageClient(
  context: vscode.ExtensionContext,
): ProtolsLanguageClient {
  const documentSelector = [
    { language: "protobuf", scheme: "file" },
    { language: "protobuf", scheme: "proto" },
  ]

  const c = new ProtolsLanguageClient(
    "protobuf",
    "protols",
    {
      command: path.join(process.env.HOME, "go", "bin", "protols"),
      args: ["serve"],
      transport: TransportKind.pipe,
    } as ServerOptions,
    {
      initializationOptions: {},
      documentSelector,
      synchronize: {
        fileEvents: vscode.workspace.createFileSystemWatcher("**/*.proto"),
      },
      revealOutputChannelOn: RevealOutputChannelOn.Never,
      outputChannel: vscode.window.createOutputChannel(
        "Protobuf Language Server",
      ),
      markdown: {
        isTrusted: true,
        supportHtml: true,
      },
    } as LanguageClientOptions,
  )
  return c
}
