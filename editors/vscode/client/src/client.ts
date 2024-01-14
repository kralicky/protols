import * as vscode from "vscode"
import * as which from "which"
import * as path from "path"
import {
  LanguageClientOptions,
  RevealOutputChannelOn,
} from "vscode-languageclient"
import {
  LanguageClient,
  ServerOptions,
  TransportKind,
} from "vscode-languageclient/node"

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
    // look up the document version for this uri
    return this.sendRequest(
      "workspace/executeCommand",
      {
        command: "protols/synthetic-file-contents",
        arguments: [{ uri: uri.toString() }],
      },
      token,
    ).then((result: string) => {
      return result
    })
  }
}

async function lookPath(cmd: string): Promise<string> {
  try {
    return await which(cmd)
  } catch (e) {
    return null
  }
}

export async function buildLanguageClient(
  context: vscode.ExtensionContext,
): Promise<ProtolsLanguageClient> {
  const documentSelector = [
    { language: "protobuf", scheme: "file" },
    { language: "protobuf", scheme: "proto" },
  ]

  let binaryPath: string | undefined = vscode.workspace
    .getConfiguration("protols")
    .get("alternateBinaryPath")

  if (!binaryPath) {
    binaryPath =
      (await lookPath("protols")) ||
      path.join(process.env.HOME, "go", "bin", "protols")
  }
  const c = new ProtolsLanguageClient(
    "protobuf",
    "protols",
    {
      command: binaryPath,
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
