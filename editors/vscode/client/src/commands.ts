import * as vscode from "vscode"

export interface SelectRangeParams {
  selectRange?: vscode.Range
  revealRange?: vscode.Range
  highlightRevealedRange: boolean
}

export function initCommands(context: vscode.ExtensionContext) {
  context.subscriptions.push(
    vscode.commands.registerCommand(
      "protols.api.selectRange",
      async (params: SelectRangeParams) => {
        const editor = vscode.window.activeTextEditor
        if (!editor) {
          return
        }
        if (params.revealRange) {
          editor.revealRange(
            params.revealRange,
            vscode.TextEditorRevealType.InCenterIfOutsideViewport,
          )
        }
        if (params.selectRange) {
          editor.selection = new vscode.Selection(
            params.selectRange.start,
            params.selectRange.end,
          )
        }
        const decoration = vscode.window.createTextEditorDecorationType({
          backgroundColor: new vscode.ThemeColor(
            "editor.findMatchHighlightBackground",
          ),
        })
        if (params.highlightRevealedRange)
          editor.setDecorations(decoration, [params.revealRange])
        setTimeout(() => {
          decoration.dispose()
        }, 500)
      },
    ),
  )
}
