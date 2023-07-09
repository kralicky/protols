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

export class RaguLanguageClient extends LanguageClient {
	constructor(
		id: string,
		name: string,
		serverOptions: ServerOptions,
		clientOptions: LanguageClientOptions,
	) {
		super(id, name, serverOptions, clientOptions);
	}
}

export function buildLanguageClient(
	context: ExtensionContext,
): RaguLanguageClient {
	const documentSelector = [
		{ language: 'protobuf', scheme: 'file' },
	];

	const c = new RaguLanguageClient(
		'protobuf',
		'protols',
		{
			command: '/home/joe/protols/protols/protols',
			args: [],
			transport: TransportKind.pipe,
		} as ServerOptions,
		{
			initializationOptions: {},
			documentSelector,
			synchronize: {fileEvents: workspace.createFileSystemWatcher('**/*.proto')},
			revealOutputChannelOn: RevealOutputChannelOn.Info,
			outputChannel: vscode.window.createOutputChannel('Protobuf Language Server'),
		} as LanguageClientOptions,
	);
	return c;
}

let client: LanguageClient;

export function activate(context: ExtensionContext) {
	const client = buildLanguageClient(context);
	// Start the client. This will also launch the server
	client.start();
}

export function deactivate(): Thenable<void> | undefined {
	if (!client) {
		return undefined;
	}
	return client.stop();
}
