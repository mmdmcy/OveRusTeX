package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const defaultTemplate = `\documentclass{article}
\usepackage[utf8]{inputenc}
\usepackage[T1]{fontenc}

\title{OveRusTeX}
\author{}
\date{\today}

\begin{document}
\maketitle

Hello from OveRusTeX.

\end{document}
`

const waitingLabel = "waiting for build"

// App holds what the editor has open and runs its commands. Every event for
// the page goes through emit, in order.
type App struct {
	mu            sync.Mutex
	currentFile   string // "" while untitled
	workspaceRoot string
	activeBuildID uint64
	previewPDF    string

	cmdMu  sync.Mutex // one command at a time, like the Rust IPC handler
	emit   func(any)
	builds sync.WaitGroup
}

func newApp(emit func(any)) *App {
	return &App{workspaceRoot: defaultWorkspaceRoot(), emit: emit}
}

// The events below keep the Rust version's names and fields.

type documentLoadedEvent struct {
	Type             string           `json:"type"`
	FileName         string           `json:"file_name"`
	DisplayPath      string           `json:"display_path"`
	Contents         string           `json:"contents"`
	PreviewURL       *string          `json:"preview_url"`
	PreviewLabel     string           `json:"preview_label"`
	Note             string           `json:"note"`
	WorkspaceRoot    string           `json:"workspace_root"`
	WorkspaceEntries []WorkspaceEntry `json:"workspace_entries"`
}

type documentSavedEvent struct {
	Type             string           `json:"type"`
	FileName         string           `json:"file_name"`
	DisplayPath      string           `json:"display_path"`
	Note             string           `json:"note"`
	PreviewURL       *string          `json:"preview_url"`
	PreviewLabel     string           `json:"preview_label"`
	WorkspaceRoot    string           `json:"workspace_root"`
	WorkspaceEntries []WorkspaceEntry `json:"workspace_entries"`
}

type buildStartedEvent struct {
	Type   string `json:"type"`
	Detail string `json:"detail"`
	Log    string `json:"log"`
}

type buildFinishedEvent struct {
	Type             string           `json:"type"`
	Success          bool             `json:"success"`
	Detail           string           `json:"detail"`
	Log              string           `json:"log"`
	PreviewURL       *string          `json:"preview_url"`
	PreviewLabel     string           `json:"preview_label"`
	ExportedPDFPath  *string          `json:"exported_pdf_path"`
	WorkspaceRoot    string           `json:"workspace_root"`
	WorkspaceEntries []WorkspaceEntry `json:"workspace_entries"`
}

type statusEvent struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Detail  string `json:"detail"`
	Log     string `json:"log"`
}

// pickEvent asks the page for a path where the Rust version opened a native
// file dialog. The page answers by sending Cmd again with "path" set.
type pickEvent struct {
	Type          string `json:"type"`
	Cmd           string `json:"cmd"`
	Mode          string `json:"mode"` // open_file, open_folder, save_tex, save_pdf
	StartDir      string `json:"start_dir"`
	SuggestedName string `json:"suggested_name"`
}

type ipcCommand struct {
	Cmd      string  `json:"cmd"`
	Path     *string `json:"path"`
	Contents *string `json:"contents"`
}

func fileName(path string) string {
	if path == "" {
		return "untitled.tex"
	}
	return filepath.Base(path)
}

func displayPath(path string) string {
	if path == "" {
		return "Untitled"
	}
	return path
}

func previewURL() *string {
	u := fmt.Sprintf("/preview.pdf?ts=%d", unixMillis())
	return &u
}

// Handle runs one message from the page. An error becomes a status event,
// as in the Rust version.
func (a *App) Handle(message []byte) {
	a.cmdMu.Lock()
	defer a.cmdMu.Unlock()
	if err := a.handle(message); err != nil {
		a.emit(statusEvent{Type: "status", Message: "Something went wrong", Detail: outerMessage(err), Log: err.Error()})
	}
}

func (a *App) handle(message []byte) error {
	var cmd ipcCommand
	if err := json.Unmarshal(message, &cmd); err != nil {
		return wrapf(err, "invalid IPC payload")
	}
	needContents := func() (string, error) {
		if cmd.Contents == nil {
			return "", wrapf(errors.New("missing field `contents`"), "invalid IPC payload")
		}
		return *cmd.Contents, nil
	}
	switch cmd.Cmd {
	case "ready":
		return a.ready()
	case "new_document":
		a.mu.Lock()
		a.currentFile = ""
		a.previewPDF = ""
		root := a.workspaceRoot
		a.mu.Unlock()
		return a.emitLoaded("", root, defaultTemplate, "Started a fresh LaTeX buffer.")
	case "open_folder":
		return a.openFolder(cmd.Path)
	case "open_file":
		return a.openFile(cmd.Path)
	case "open_workspace_file":
		if cmd.Path == nil {
			return wrapf(errors.New("missing field `path`"), "invalid IPC payload")
		}
		return a.openWorkspaceFile(*cmd.Path)
	case "save", "save_as":
		contents, err := needContents()
		if err != nil {
			return err
		}
		return a.save(cmd.Cmd, contents, cmd.Path)
	case "run", "export_pdf":
		contents, err := needContents()
		if err != nil {
			return err
		}
		return a.startBuild(cmd.Cmd, contents, "")
	case "save_pdf_as":
		contents, err := needContents()
		if err != nil {
			return err
		}
		if cmd.Path == nil {
			a.mu.Lock()
			root := a.workspaceRoot
			a.mu.Unlock()
			a.emit(pickEvent{Type: "pick", Cmd: cmd.Cmd, Mode: "save_pdf", StartDir: root, SuggestedName: "export.pdf"})
			return nil
		}
		target, err := pickedSavePath(*cmd.Path)
		if err != nil {
			return err
		}
		return a.startBuild(cmd.Cmd, contents, target)
	}
	return wrapf(fmt.Errorf("unknown variant `%s`", cmd.Cmd), "invalid IPC payload")
}

func (a *App) ready() error {
	defaultFile := defaultRunPath()
	if _, err := os.Stat(defaultFile); err == nil {
		data, err := os.ReadFile(defaultFile)
		if err != nil {
			return wrapf(err, "failed to read startup file %s", defaultFile)
		}
		root := filepath.Dir(defaultFile)
		a.setDocument(defaultFile, root)
		return a.emitLoaded(defaultFile, root, string(data), fmt.Sprintf(
			"Loaded %s. Run builds an internal working copy without writing next to the source file.", defaultFile))
	}
	root := defaultWorkspaceRoot()
	a.setDocument("", root)
	return a.emitLoaded("", root, defaultTemplate,
		"New document ready. Press Run to build a preview without saving the .tex file.")
}

func (a *App) setDocument(file, root string) {
	a.mu.Lock()
	a.currentFile = file
	a.workspaceRoot = root
	a.previewPDF = ""
	a.mu.Unlock()
}

func (a *App) emitLoaded(file, root, contents, note string) error {
	entries, err := workspaceEntries(root)
	if err != nil {
		return err
	}
	a.emit(documentLoadedEvent{
		Type:             "document_loaded",
		FileName:         fileName(file),
		DisplayPath:      displayPath(file),
		Contents:         contents,
		PreviewLabel:     waitingLabel,
		Note:             note,
		WorkspaceRoot:    root,
		WorkspaceEntries: entries,
	})
	return nil
}

func (a *App) workspace() (root, file string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.workspaceRoot, a.currentFile
}

func (a *App) openFolder(picked *string) error {
	root, _ := a.workspace()
	if picked == nil {
		a.emit(pickEvent{Type: "pick", Cmd: "open_folder", Mode: "open_folder", StartDir: root})
		return nil
	}
	folder, err := pickedPath(*picked)
	if err != nil {
		return err
	}
	if info, err := os.Stat(folder); err != nil || !info.IsDir() {
		return fmt.Errorf("not a folder: %s", folder)
	}
	mainTex := filepath.Join(folder, "main.tex")
	if _, err := os.Stat(mainTex); err == nil {
		data, err := os.ReadFile(mainTex)
		if err != nil {
			return wrapf(err, "failed to read %s", mainTex)
		}
		a.setDocument(mainTex, folder)
		return a.emitLoaded(mainTex, folder, string(data), fmt.Sprintf("Opened workspace %s and loaded main.tex.", folder))
	}
	a.setDocument("", folder)
	return a.emitLoaded("", folder, defaultTemplate, fmt.Sprintf("Opened workspace %s.", folder))
}

func (a *App) openFile(picked *string) error {
	root, _ := a.workspace()
	if picked == nil {
		a.emit(pickEvent{Type: "pick", Cmd: "open_file", Mode: "open_file", StartDir: root})
		return nil
	}
	path, err := pickedPath(*picked)
	if err != nil {
		return err
	}
	return a.loadFile(root, path)
}

func (a *App) openWorkspaceFile(rel string) error {
	root, _ := a.workspace()
	path, err := workspaceFilePath(root, rel)
	if err != nil {
		return err
	}
	return a.loadFile(root, path)
}

func (a *App) loadFile(currentRoot, path string) error {
	data, err := readRegularFile(path)
	if err != nil {
		return wrapf(err, "failed to read %s", path)
	}
	root := workspaceRootForPath(currentRoot, path)
	a.setDocument(path, root)
	return a.emitLoaded(path, root, string(data), fmt.Sprintf("Opened %s.", path))
}

func readRegularFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	return os.ReadFile(path)
}

func (a *App) save(cmd, contents string, picked *string) error {
	root, current := a.workspace()
	path := current
	if cmd == "save_as" || current == "" {
		if picked == nil {
			suggested := "main.tex"
			if current != "" {
				suggested = filepath.Base(current)
			}
			a.emit(pickEvent{Type: "pick", Cmd: cmd, Mode: "save_tex", StartDir: root, SuggestedName: suggested})
			return nil
		}
		var err error
		if path, err = pickedSavePath(*picked); err != nil {
			return err
		}
	}
	if err := saveDocument(path, contents); err != nil {
		return err
	}
	root = workspaceRootForPath(root, path)
	a.mu.Lock()
	a.currentFile = path
	a.workspaceRoot = root
	hasPreview := a.previewPDF != ""
	a.mu.Unlock()
	ev := documentSavedEvent{
		Type:          "document_saved",
		FileName:      fileName(path),
		DisplayPath:   displayPath(path),
		Note:          fmt.Sprintf("Saved %s.", path),
		PreviewLabel:  waitingLabel,
		WorkspaceRoot: root,
	}
	if hasPreview {
		ev.PreviewURL = previewURL()
		ev.PreviewLabel = "rendered"
	}
	entries, err := workspaceEntries(root)
	if err != nil {
		return err
	}
	ev.WorkspaceEntries = entries
	a.emit(ev)
	return nil
}

// startBuild runs a build in the background: run previews, export_pdf
// writes next to the .tex, save_pdf_as writes to target.
func (a *App) startBuild(cmd, contents, target string) error {
	a.mu.Lock()
	a.activeBuildID++
	action, started := actionPreview, buildStartedEvent{Type: "build_started", Detail: "Preview build in progress"}
	switch cmd {
	case "export_pdf":
		action, target = actionExportDefault, defaultExportPDFPath(a.workspaceRoot, a.currentFile)
		started.Detail, started.Log = "Exporting PDF", "Building the current buffer and exporting a PDF copy..."
	case "save_pdf_as":
		action = actionExportTo
		started.Detail, started.Log = "Exporting PDF", "Building the current buffer and saving the PDF to your chosen location..."
	}
	plan := buildPlanFor(a.activeBuildID, a.workspaceRoot, a.currentFile, contents, action, target)
	a.mu.Unlock()
	if action == actionPreview {
		started.Log = fmt.Sprintf("Building %s as an internal working copy ...", plan.sourceFileName)
	}
	a.emit(started)
	a.builds.Add(1)
	go func() {
		defer a.builds.Done()
		a.finishBuild(compileDocument(plan))
	}()
	return nil
}

func (a *App) finishBuild(outcome buildOutcome) {
	a.mu.Lock()
	ev := buildFinishedEvent{
		Type:          "build_finished",
		Success:       outcome.success,
		Detail:        outcome.detail,
		Log:           outcome.log,
		PreviewLabel:  "build failed",
		WorkspaceRoot: a.workspaceRoot,
	}
	if outcome.success {
		ev.PreviewLabel = "rendered"
		if a.activeBuildID == outcome.buildID && outcome.previewPDF != "" {
			a.previewPDF = outcome.previewPDF
			ev.PreviewURL = previewURL()
		}
	}
	if outcome.exportedPDFPath != "" {
		p := outcome.exportedPDFPath
		ev.ExportedPDFPath = &p
	}
	a.mu.Unlock()
	entries, err := workspaceEntries(ev.WorkspaceRoot)
	if err != nil {
		entries = []WorkspaceEntry{}
	}
	ev.WorkspaceEntries = entries
	a.emit(ev)
}

// PreviewPDF is the file /preview.pdf serves, or "".
func (a *App) PreviewPDF() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.previewPDF
}

// pickedPath checks a path the in-app picker sent: it must be absolute, and
// it is cleaned.
func pickedPath(p string) (string, error) {
	if p == "" || !filepath.IsAbs(p) || strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("the picker sent a path that is not absolute: %q", p)
	}
	return filepath.Clean(p), nil
}

// pickedSavePath is a picked path to write: its folder must exist and the
// path must not be a folder.
func pickedSavePath(p string) (string, error) {
	path, err := pickedPath(p)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(filepath.Dir(path)); err != nil || !info.IsDir() {
		return "", fmt.Errorf("the folder %s does not exist", filepath.Dir(path))
	}
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return "", fmt.Errorf("%s is a folder", path)
	}
	return path, nil
}
