# OverusTeX

A fast, small LaTeX editor that runs in your browser, written in Go with the standard library only:

- a clean split layout with workspace explorer, editor, and preview
- a one-click LaTeX build flow for previewing and exporting PDFs
- internal preview/build caches managed by the app instead of hidden folders next to your `.tex`
- one self-contained program for Windows, macOS, and Linux, with nothing to install besides TeX

## Features

- a VS Code-like explorer sidebar on the left
- a fast plain-text editor in the middle
- a PDF preview pane on the right
- `Run` builds an unsaved working copy instead of silently saving your `.tex`
- opening `.tex` files from nested folders keeps the current workspace root stable
- quick `Export PDF` and `Save PDF As` actions
- build output at the bottom
- `latexmk` when Perl is available, otherwise `pdflatex` twice; MiKTeX-friendly

## How it runs

OverusTeX is one program that serves its editor page on `127.0.0.1` at a free port and opens it in your default browser. It prints the address too, so you can open it yourself (`--no-browser` skips opening a browser). Open and save dialogs are a small file picker inside the page, the same on every OS.

It stops when you press **Quit**, or when no OverusTeX tab has been open for a minute (`--idle 5m` changes that). An open tab keeps a live connection to the program; closing the tab ends it.

Security: the page and every request need a random token that only the address OverusTeX opens carries (it becomes an HttpOnly, SameSite=Strict cookie), and requests with a foreign `Host` or `Origin` are refused, so other web pages in the same browser cannot reach your files through it. The explorer opens files inside the workspace only (no `..` or symlink escapes); the picker goes wherever you browse, as a native dialog would.

## Run

```sh
go run .
```

Options: `--no-browser`, `--port N` (default: a free port), `--idle DURATION` (default `1m`).

## Build

```sh
go build -o overustex .
```

On Windows, `go build -o overustex.exe .` gives a program that shows its address in a console window; `go build -ldflags -H=windowsgui -o overustex.exe .` gives one without a console window.

Cross-compiling needs no C toolchain, for example `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o overustex.exe .`.

## Test

```sh
go vet ./...
go test ./...
```

The tests need no TeX: they put small stand-in `latexmk`, `perl` and `pdflatex` scripts on `PATH` (POSIX systems only; on Windows those tests are skipped).

## Notes

- `Run` does not save your `.tex` file. It compiles a temporary working copy inside OverusTeX's cache storage instead of creating hidden folders next to your source files.
- OverusTeX keeps short-term snapshot backups in that internal cache and prunes old entries automatically. The cache is `OveRusTeX/documents` in your user cache folder (`~/.cache` or `$XDG_CACHE_HOME` on Linux, `~/Library/Caches` on macOS, `%LocalAppData%` on Windows).
- At start it opens `OveRusTeX/main.tex` in your Documents folder when that file exists.
- `Save` writes the current file. `Save As` lets you choose a different `.tex` file.
- `Export PDF` writes a PDF next to the current `.tex` file, or to `main.pdf` in the workspace when the file is still untitled.
- `Save PDF As` lets you choose a PDF target explicitly.
- You can drag file paths from the explorer into the editor.
- `Ctrl+S` saves.
- `Ctrl+Enter` or `F5` builds.

## Platform Status

- Windows: the main target. It builds and passes `go vet` for Windows (`GOOS=windows`); the Go version has not yet been run on Windows.
- macOS: builds and passes `go vet` for macOS (`GOOS=darwin`); runtime behaviour still needs real-world testing.
- Linux: builds, its tests pass, and it has been driven end to end in a browser with stand-in TeX tools; a run with a real TeX installation still needs testing.

CI is turned off (its workflow file is from the Rust version): run `go vet ./...` and `go test ./...` before a change.

Earlier versions were written in Rust with a native window (tao and wry); they remain in the git history.
