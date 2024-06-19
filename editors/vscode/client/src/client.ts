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
import semver = require("semver")
import fs = require("fs")

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
        command: "protols/syntheticFileContents",
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

async function findProtolsBinary(): Promise<string> {
  return (
    (await lookPath("protols")) ||
    path.join(process.env.HOME, "go", "bin", "protols")
  )
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
    binaryPath = await findProtolsBinary()
  }
  if (!binaryExists(binaryPath)) {
    if (await promptInstall()) {
      binaryPath = await findProtolsBinary()
    } else {
      throw new Error("protols binary not found")
    }
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
      connectionOptions: {
        maxRestartCount: 1,
      },
      markdown: {
        isTrusted: true,
        supportHtml: true,
      },
    } as LanguageClientOptions,
  )
  return c
}

const protolsToolInfo = {
  name: "protols",
  importPath: "github.com/kralicky/protols/cmd/protols",
  modulePath: "github.com/kralicky/protols",
  description: "Protobuf Language Server",
  minimumGoVersion: semver.coerce("1.22"),
}

async function promptInstall(): Promise<boolean> {
  const selected = await vscode.window.showInformationMessage(
    `${protolsToolInfo.name} not found in $PATH. Install it with "go install"?`,
    "Install",
    "Cancel",
  )
  if (selected !== "Install") {
    return false
  }
  try {
    await vscode.commands.executeCommand("go.tools.install", [protolsToolInfo])
    return true
  } catch (error) {
    vscode.window.showErrorMessage(
      `Failed to install ${protolsToolInfo.name}: ${error}`,
    )
    return false
  }
}

function binaryExists(path: string): boolean {
  try {
    fs.accessSync(
      path,
      fs.constants.F_OK | fs.constants.R_OK | fs.constants.X_OK,
    )
    return true
  } catch {
    return false
  }
}
