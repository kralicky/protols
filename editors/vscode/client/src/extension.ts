/* --------------------------------------------------------------------------------------------
 * Copyright (c) Microsoft Corporation. All rights reserved.
 * Licensed under the MIT License. See License.txt in the project root for license information.
 * ------------------------------------------------------------------------------------------ */

import * as path from 'path';
import { workspace, ExtensionContext } from 'vscode';
import vscode = require('vscode');
import {
	CancellationToken,
	CloseAction,
	ConfigurationParams,
	ConfigurationRequest,
	ErrorAction,
	ExecuteCommandParams,
	ExecuteCommandRequest,
	ExecuteCommandSignature,
	HandleDiagnosticsSignature,
	InitializeError,
	InitializeResult,
	LanguageClientOptions,
	Message,
	ProgressToken,
	ProvideCodeLensesSignature,
	ProvideCompletionItemsSignature,
	ProvideDocumentFormattingEditsSignature,
	ResponseError,
	RevealOutputChannelOn
} from 'vscode-languageclient';
import {
	LanguageClient,
	ServerOptions,
	TransportKind,
	SocketTransport,
} from 'vscode-languageclient/node';

export class RaguLanguageClient extends LanguageClient implements vscode.TextDocumentContentProvider  {
	constructor(
		id: string,
		name: string,
		serverOptions: ServerOptions,
		clientOptions: LanguageClientOptions,
	) {
		super(id, name, serverOptions, clientOptions);
	}
	onDidChange?: vscode.Event<vscode.Uri>;
	provideTextDocumentContent(uri: vscode.Uri, token: vscode.CancellationToken): vscode.ProviderResult<string> {
		return this.sendRequest('protols/synthetic-file-contents', uri.toString()).then((result: string) => {
			return result;
		});
	}
}

export function buildLanguageClient(
	context: ExtensionContext,
): RaguLanguageClient {
	const documentSelector = [
		{ language: 'protobuf', scheme: 'file' },
		{ language: 'protobuf', scheme: 'proto' }
	];

	const c = new RaguLanguageClient(
		'protobuf',
		'protols',
		{
			command: path.join(process.env.HOME, 'go', 'bin', 'protols'),
			args: ['serve'],
			transport: TransportKind.pipe,
		} as ServerOptions,
		{
			initializationOptions: {},
			documentSelector,
			synchronize: {fileEvents: workspace.createFileSystemWatcher('**/*.proto')},
			revealOutputChannelOn: RevealOutputChannelOn.Never,
			outputChannel: vscode.window.createOutputChannel('Protobuf Language Server'),
		} as LanguageClientOptions,
	);
	return c;
}

let client: LanguageClient;

export function activate(context: ExtensionContext) {
	const client = buildLanguageClient(context);
	workspace.registerTextDocumentContentProvider('proto', client);
	// Start the client. This will also launch the server
	client.start();

	context.subscriptions.push(
		vscode.commands.registerCommand('protols.restart', async () => {
			try {
				await client.restart();
			} catch (e) {
				// if it's already stopped, restart will throw an error.
				await client.start();
			}
		},
		vscode.commands.registerCommand('protols.stop', async () => {
			await client.stop();
		})
	));
}

export function deactivate(): Thenable<void> | undefined {
	if (!client) {
		return undefined;
	}
	return client.stop();
}
