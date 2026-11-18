package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	historyLimit       = 16
	previewLimit       = 6
	documentCacheLimit = 24
)

type buildAction int

const (
	actionPreview buildAction = iota
	actionExportDefault
	actionExportTo
)

type buildPlan struct {
	buildID        uint64
	sourceDir      string
	cacheDir       string
	tempSourcePath string
	sourceFileName string
	jobName        string
	contents       string
	action         buildAction
	exportTarget   string
}

type buildOutcome struct {
	buildID         uint64
	success         bool
	detail          string
	log             string
	previewPDF      string
	exportedPDFPath string
}

// commandOutput is what a finished TeX run left behind.
type commandOutput struct {
	stdout, stderr []byte
	success        bool
	status         string
}

func buildPlanFor(buildID uint64, workspaceRoot, currentFile, contents string, action buildAction, exportTarget string) buildPlan {
	sourceFileName, stem, sourceDir := "main.tex", "main", workspaceRoot
	if currentFile != "" {
		sourceFileName = filepath.Base(currentFile)
		stem, _, _ = splitStem(sourceFileName)
		sourceDir = filepath.Dir(currentFile)
	}
	cacheDir := documentCacheDir(workspaceRoot, currentFile)
	return buildPlan{
		buildID:        buildID,
		sourceDir:      sourceDir,
		cacheDir:       cacheDir,
		tempSourcePath: filepath.Join(cacheDir, "source", sourceFileName),
		sourceFileName: sourceFileName,
		jobName:        sanitizeJobName(stem),
		contents:       contents,
		action:         action,
		exportTarget:   exportTarget,
	}
}

// defaultExportPDFPath is the .tex file with .pdf, or main.pdf in the
// workspace for an untitled document.
func defaultExportPDFPath(workspaceRoot, currentFile string) string {
	if currentFile != "" {
		return withExtension(currentFile, "pdf")
	}
	return filepath.Join(workspaceRoot, "main.pdf")
}

func buildOutputDir(cacheDir string) string   { return filepath.Join(cacheDir, "build") }
func previewOutputDir(cacheDir string) string { return filepath.Join(cacheDir, "preview") }
func historyDir(cacheDir string) string       { return filepath.Join(cacheDir, "history") }

func unixMillis() int64 { return time.Now().UnixMilli() }

// compileDocument builds the working copy: a snapshot into history, the copy
// into the cache, latexmk when perl runs (pdflatex twice otherwise or when
// latexmk is missing), then a preview snapshot and the export copy.
func compileDocument(plan buildPlan) buildOutcome {
	outcome, err := compile(plan)
	if err != nil {
		return buildOutcome{
			buildID: plan.buildID,
			detail:  "Build failed before compilation completed",
			log:     err.Error(),
		}
	}
	return outcome
}

func compile(plan buildPlan) (buildOutcome, error) {
	parent := documentCacheParent()
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return buildOutcome{}, wrapf(err, "failed to create %s", parent)
	}
	if err := pruneOldDirectories(parent, documentCacheLimit); err != nil {
		return buildOutcome{}, err
	}
	for _, dir := range []string{plan.sourceDir, plan.cacheDir, buildOutputDir(plan.cacheDir), previewOutputDir(plan.cacheDir)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return buildOutcome{}, wrapf(err, "failed to create %s", dir)
		}
	}
	if err := writeSnapshot(plan.cacheDir, plan.jobName, plan.contents); err != nil {
		return buildOutcome{}, err
	}
	if err := saveDocument(plan.tempSourcePath, plan.contents); err != nil {
		return buildOutcome{}, err
	}

	var (
		output commandOutput
		engine string
		err    error
	)
	if canUseLatexmk() {
		engine = "latexmk"
		output, err = runCommand(texCommand(plan.sourceDir, "latexmk",
			"-pdf",
			"-interaction=nonstopmode",
			"-synctex=1",
			"-file-line-error",
			"-outdir="+buildOutputDir(plan.cacheDir),
			"-jobname="+plan.jobName,
			plan.tempSourcePath,
		))
		if isNotFound(err) {
			engine = "pdflatex"
			if output, err = runPdflatexTwice(plan.sourceDir, plan.cacheDir, plan.tempSourcePath, plan.jobName); err != nil {
				return buildOutcome{}, err
			}
		} else if err != nil {
			return buildOutcome{}, wrapf(err, "failed to launch latexmk")
		}
	} else {
		engine = "pdflatex"
		if output, err = runPdflatexTwice(plan.sourceDir, plan.cacheDir, plan.tempSourcePath, plan.jobName); err != nil {
			return buildOutcome{}, err
		}
	}

	_ = os.Remove(plan.tempSourcePath)

	log := formatCommandOutput(engine, output)
	builtPDF := filepath.Join(buildOutputDir(plan.cacheDir), plan.jobName+".pdf")

	if _, statErr := os.Stat(builtPDF); !output.success || statErr != nil {
		return buildOutcome{buildID: plan.buildID, detail: engine + " reported an error", log: log}, nil
	}

	previewPDF, err := createPreviewSnapshot(plan.cacheDir, plan.jobName, builtPDF)
	if err != nil {
		return buildOutcome{}, err
	}
	exported := ""
	if plan.action != actionPreview {
		target := plan.exportTarget
		if dir := filepath.Dir(target); dir != target {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return buildOutcome{}, wrapf(err, "failed to create export directory %s", dir)
			}
		}
		if err := copyFile(builtPDF, target); err != nil {
			return buildOutcome{}, wrapf(err, "failed to export PDF to %s", target)
		}
		exported = target
	}
	detail := engine + " finished successfully"
	if exported != "" {
		detail += " and exported " + exported
	}
	return buildOutcome{
		buildID:         plan.buildID,
		success:         true,
		detail:          detail,
		log:             log,
		previewPDF:      previewPDF,
		exportedPDFPath: exported,
	}, nil
}

func canUseLatexmk() bool {
	cmd := exec.Command("perl", "-v")
	hideWindow(cmd)
	return cmd.Run() == nil
}

func runPdflatexTwice(sourceDir, cacheDir, sourcePath, jobName string) (commandOutput, error) {
	args := []string{
		"-interaction=nonstopmode",
		"-synctex=1",
		"-file-line-error",
		"-output-directory=" + buildOutputDir(cacheDir),
		"-jobname=" + jobName,
		sourcePath,
	}
	first, err := runCommand(texCommand(sourceDir, "pdflatex", args...))
	if err != nil {
		return commandOutput{}, wrapf(err, "failed to launch pdflatex")
	}
	if !first.success {
		return first, nil
	}
	second, err := runCommand(texCommand(sourceDir, "pdflatex", args...))
	if err != nil {
		return commandOutput{}, wrapf(err, "failed to launch pdflatex")
	}
	return commandOutput{
		stdout:  joinOutput(first.stdout, second.stdout),
		stderr:  joinOutput(first.stderr, second.stderr),
		success: second.success,
		status:  second.status,
	}, nil
}

func joinOutput(a, b []byte) []byte {
	out := append(append([]byte{}, a...), "\n\n"...)
	return append(out, b...)
}

// texCommand runs name in the document's folder with that folder in front
// of TEXINPUTS, BIBINPUTS and BSTINPUTS. The trailing separator stays, so
// TeX still searches its own paths after it.
func texCommand(sourceDir, name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.Dir = sourceDir
	env := os.Environ()
	for _, key := range []string{"TEXINPUTS", "BIBINPUTS", "BSTINPUTS"} {
		env = append(env, key+"="+sourceDir+string(os.PathListSeparator)+os.Getenv(key))
	}
	cmd.Env = env
	hideWindow(cmd)
	return cmd
}

// runCommand captures a command's output. A non-zero exit is an output, not
// an error; only failing to start is an error.
func runCommand(cmd *exec.Cmd) (commandOutput, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return commandOutput{}, err
	}
	out := commandOutput{stdout: stdout.Bytes(), stderr: stderr.Bytes(), success: err == nil}
	if cmd.ProcessState != nil {
		out.status = exitStatusText(cmd.ProcessState)
	}
	return out, nil
}

func isNotFound(err error) bool {
	return errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist)
}

func formatCommandOutput(label string, out commandOutput) string {
	sections := []string{"$ " + label}
	if s := normalizeOutput(out.stdout); s != "" {
		sections = append(sections, s)
	}
	if s := normalizeOutput(out.stderr); s != "" {
		sections = append(sections, "[stderr]\n"+s)
	}
	if !out.success {
		sections = append(sections, "[exit status] "+out.status)
	}
	return strings.Join(sections, "\n\n")
}

func normalizeOutput(b []byte) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.ToValidUTF8(string(b), "�"), "\r\n", "\n"))
}

func saveDocument(path, contents string) error {
	if dir := filepath.Dir(path); dir != path {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return wrapf(err, "failed to create %s", dir)
		}
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		return wrapf(err, "failed to write %s", path)
	}
	return nil
}

func writeSnapshot(cacheDir, jobName, contents string) error {
	dir := historyDir(cacheDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return wrapf(err, "failed to create %s", dir)
	}
	name := fmt.Sprintf("%020d-%s.tex", unixMillis(), jobName)
	if err := saveDocument(filepath.Join(dir, name), contents); err != nil {
		return err
	}
	return pruneOldFiles(dir, historyLimit)
}

func createPreviewSnapshot(cacheDir, jobName, builtPDF string) (string, error) {
	dir := previewOutputDir(cacheDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", wrapf(err, "failed to create %s", dir)
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%d.pdf", jobName, unixMillis()))
	if err := copyFile(builtPDF, path); err != nil {
		return "", wrapf(err, "failed to create preview snapshot %s from %s", path, builtPDF)
	}
	if err := pruneOldFiles(dir, previewLimit); err != nil {
		return "", err
	}
	return path, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// oldestFirst sorts entries by modification time, unreadable ones first.
func oldestFirst(dir string, entries []os.DirEntry) {
	mtime := func(e os.DirEntry) time.Time {
		if info, err := e.Info(); err == nil {
			return info.ModTime()
		}
		return time.Time{}
	}
	sort.SliceStable(entries, func(i, j int) bool { return mtime(entries[i]).Before(mtime(entries[j])) })
}

// pruneOldFiles keeps the newest keep entries of dir and removes the older
// regular files.
func pruneOldFiles(dir string, keep int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return wrapf(err, "failed to read %s", dir)
	}
	if len(entries) <= keep {
		return nil
	}
	oldestFirst(dir, entries)
	for _, e := range entries[:len(entries)-keep] {
		path := filepath.Join(dir, e.Name())
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			_ = os.Remove(path)
		}
	}
	return nil
}

// pruneOldDirectories keeps the newest keep folders of dir.
func pruneOldDirectories(dir string, keep int) error {
	if _, err := os.Stat(dir); err != nil {
		return nil
	}
	all, err := os.ReadDir(dir)
	if err != nil {
		return wrapf(err, "failed to read %s", dir)
	}
	var dirs []os.DirEntry
	for _, e := range all {
		if e.IsDir() {
			dirs = append(dirs, e)
		}
	}
	if len(dirs) <= keep {
		return nil
	}
	oldestFirst(dir, dirs)
	for _, e := range dirs[:len(dirs)-keep] {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
	return nil
}
