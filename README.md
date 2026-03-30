# OverusTeX

A fast desktop LaTeX editor in Rust with:

- a clean split layout with workspace explorer, editor, and preview
- a native desktop shell with a lightweight webview UI
- a one-click LaTeX build flow for previewing and exporting PDFs
- a short-term snapshot history in `.overustex/history`
- CI for Windows, macOS, and Linux builds

## Features

- a VS Code-like explorer sidebar on the left
- a fast plain-text editor in the middle
- a PDF preview pane on the right
- `Run` builds an unsaved working copy instead of silently saving your `.tex`
- an embedded PDF preview on the right
- quick `Export PDF` and `Save PDF As` actions
- build output at the bottom

It is built with `tao`, `wry`, and MiKTeX-friendly command execution, keeping the stack intentionally small and responsive.

## Platform Status

- Windows: actively tested in development and the main target right now.
- macOS: intended to work through `wry` + WebKit and covered by CI compile checks.
- Linux: intended to work through `wry` + WebKitGTK and covered by CI compile checks.

Runtime behavior on macOS and Linux still needs real-world testing by contributors. The codebase is being kept cross-platform where practical, and the CI matrix is there to keep that direction honest.

## Run

```powershell
cargo run
```

## Build

```powershell
cargo build --release
```

The binary will be at:

```text
target\release\overustex.exe
```

## Notes

- `Run` does not save your `.tex` file. It compiles a temporary working copy in a hidden `.overustex` folder inside the workspace.
- OverusTeX keeps short-term snapshot backups in `.overustex\history`.
- `Save` writes the current file. `Save As` lets you choose a different `.tex` file.
- `Export PDF` writes a PDF next to the current `.tex` file, or to `main.pdf` in the workspace when the file is still untitled.
- `Save PDF As` lets you choose a PDF target explicitly.
- You can drag file paths from the explorer into the editor.
- `Ctrl+S` saves.
- `Ctrl+Enter` or `F5` builds.

## Linux Dependencies

According to the `wry` platform notes, Linux builds require WebKitGTK. On Debian or Ubuntu systems that means at least:

```bash
sudo apt install libgtk-3-dev libwebkit2gtk-4.1-dev
```
