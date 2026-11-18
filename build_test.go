package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The stand-ins record their name, folder, TEXINPUTS and arguments (one per
// line) to $STANDIN_LOG and write a small PDF where the real ones would.
const standinTeX = `#!/bin/sh
{ echo "== ${0##*/}"; echo "cwd=$(pwd)"; echo "TEXINPUTS=$TEXINPUTS"; echo "BIBINPUTS=$BIBINPUTS"; for a in "$@"; do echo "$a"; done; } >> "$STANDIN_LOG"
out=. job=
for a in "$@"; do
  case "$a" in
    -outdir=*) out="${a#-outdir=}" ;;
    -output-directory=*) out="${a#-output-directory=}" ;;
    -jobname=*) job="${a#-jobname=}" ;;
  esac
done
echo "This is ${0##*/} stand-in"
echo "a warning" >&2
if [ -n "$STANDIN_FAIL" ]; then exit 1; fi
printf '%%PDF-1.4 stand-in\n' > "$out/$job.pdf"
`

type standins struct {
	bin, log string
}

// setupStandins puts only the named stand-ins on PATH. perl is "ok" or
// "broken" or left out.
func setupStandins(t *testing.T, perl string, tools ...string) standins {
	t.Helper()
	skipOnWindows(t)
	dir := t.TempDir()
	s := standins{bin: filepath.Join(dir, "bin"), log: filepath.Join(dir, "calls.log")}
	if err := os.MkdirAll(s.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		if err := os.WriteFile(filepath.Join(s.bin, tool), []byte(standinTeX), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	switch perl {
	case "ok":
		os.WriteFile(filepath.Join(s.bin, "perl"), []byte("#!/bin/sh\necho perl >> \"$STANDIN_LOG\"\nexit 0\n"), 0o755)
	case "broken":
		os.WriteFile(filepath.Join(s.bin, "perl"), []byte("#!/bin/sh\nexit 2\n"), 0o755)
	}
	t.Setenv("PATH", s.bin)
	t.Setenv("STANDIN_LOG", s.log)
	t.Setenv("STANDIN_FAIL", "")
	t.Setenv("TEXINPUTS", "/existing/tex")
	t.Setenv("BIBINPUTS", "")
	return s
}

// calls splits the log into one []string per run: name, cwd, TEXINPUTS,
// BIBINPUTS, then the arguments.
func (s standins) calls(t *testing.T) [][]string {
	data, err := os.ReadFile(s.log)
	if os.IsNotExist(err) {
		return nil
	}
	var out [][]string
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if strings.HasPrefix(line, "== ") {
			out = append(out, []string{strings.TrimPrefix(line, "== ")})
		} else if line != "perl" && len(out) > 0 {
			out[len(out)-1] = append(out[len(out)-1], line)
		}
	}
	return out
}

func newDocument(t *testing.T) (root, file string) {
	root = filepath.Join(t.TempDir(), "project")
	file = filepath.Join(root, "chapters", "My Thesis.tex")
	writeFile(t, file, "\\documentclass{article}")
	return root, file
}

func TestBuildUsesLatexmkWhenPerlRuns(t *testing.T) {
	isolate(t)
	s := setupStandins(t, "ok", "latexmk", "pdflatex")
	root, file := newDocument(t)
	plan := buildPlanFor(7, root, file, "contents", actionPreview, "")
	out := compileDocument(plan)

	calls := s.calls(t)
	if len(calls) != 1 || calls[0][0] != "latexmk" {
		t.Fatalf("calls %v", calls)
	}
	build := filepath.Join(plan.cacheDir, "build")
	want := []string{"latexmk", "cwd=" + filepath.Dir(file), "TEXINPUTS=" + filepath.Dir(file) + ":/existing/tex",
		"BIBINPUTS=" + filepath.Dir(file) + ":",
		"-pdf", "-interaction=nonstopmode", "-synctex=1", "-file-line-error",
		"-outdir=" + build, "-jobname=My_Thesis", filepath.Join(plan.cacheDir, "source", "My Thesis.tex")}
	if strings.Join(calls[0], "\n") != strings.Join(want, "\n") {
		t.Fatalf("latexmk call\n%s\nwant\n%s", strings.Join(calls[0], "\n"), strings.Join(want, "\n"))
	}
	if !out.success || out.detail != "latexmk finished successfully" || out.exportedPDFPath != "" {
		t.Fatalf("outcome %+v", out)
	}
	if out.log != "$ latexmk\n\nThis is latexmk stand-in\n\n[stderr]\na warning" {
		t.Fatalf("log %q", out.log)
	}
	if !strings.HasPrefix(filepath.Base(out.previewPDF), "My_Thesis-") || filepath.Dir(out.previewPDF) != filepath.Join(plan.cacheDir, "preview") {
		t.Fatalf("preview %s", out.previewPDF)
	}
	if _, err := os.Stat(plan.tempSourcePath); !os.IsNotExist(err) {
		t.Fatal("the working copy was not removed")
	}
	history, _ := os.ReadDir(filepath.Join(plan.cacheDir, "history"))
	if len(history) != 1 || !strings.HasSuffix(history[0].Name(), "-My_Thesis.tex") || len(history[0].Name()) != 20+len("-My_Thesis.tex") {
		t.Fatalf("history %v", history)
	}
	if data, _ := os.ReadFile(file); string(data) != "\\documentclass{article}" {
		t.Fatal("Run changed the .tex file")
	}
}

func TestBuildFallsBackToPdflatexTwice(t *testing.T) {
	for _, c := range []struct{ name, perl string }{
		{"latexmk missing", "ok"},
		{"perl missing", ""},
		{"perl failing", "broken"},
	} {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			tools := []string{"pdflatex"}
			if c.perl != "ok" {
				tools = append(tools, "latexmk") // present but must not be used
			}
			s := setupStandins(t, c.perl, tools...)
			root, file := newDocument(t)
			plan := buildPlanFor(1, root, file, "x", actionPreview, "")
			out := compileDocument(plan)
			calls := s.calls(t)
			if len(calls) != 2 {
				t.Fatalf("calls %v", calls)
			}
			wantArgs := []string{"-interaction=nonstopmode", "-synctex=1", "-file-line-error",
				"-output-directory=" + filepath.Join(plan.cacheDir, "build"), "-jobname=My_Thesis", plan.tempSourcePath}
			for _, call := range calls {
				if call[0] != "pdflatex" || call[1] != "cwd="+filepath.Dir(file) || strings.Join(call[4:], " ") != strings.Join(wantArgs, " ") {
					t.Fatalf("call %v", call)
				}
			}
			if !out.success || out.detail != "pdflatex finished successfully" {
				t.Fatalf("outcome %+v", out)
			}
			want := "$ pdflatex\n\nThis is pdflatex stand-in\n\n\nThis is pdflatex stand-in\n\n[stderr]\na warning\n\n\na warning"
			if out.log != want {
				t.Fatalf("log %q", out.log)
			}
		})
	}
}

func TestBuildFailureStopsAfterFirstPdflatex(t *testing.T) {
	isolate(t)
	s := setupStandins(t, "", "pdflatex")
	t.Setenv("STANDIN_FAIL", "1")
	root, file := newDocument(t)
	out := compileDocument(buildPlanFor(1, root, file, "x", actionExportDefault, withExtension(file, "pdf")))
	if len(s.calls(t)) != 1 {
		t.Fatalf("calls %v", s.calls(t))
	}
	if out.success || out.detail != "pdflatex reported an error" || out.previewPDF != "" || out.exportedPDFPath != "" {
		t.Fatalf("outcome %+v", out)
	}
	if !strings.HasSuffix(out.log, "[exit status] exit status: 1") {
		t.Fatalf("log %q", out.log)
	}
	if _, err := os.Stat(withExtension(file, "pdf")); !os.IsNotExist(err) {
		t.Fatal("a failed build exported a PDF")
	}
}

func TestBuildWithoutAnyTeX(t *testing.T) {
	isolate(t)
	setupStandins(t, "")
	root, file := newDocument(t)
	out := compileDocument(buildPlanFor(1, root, file, "x", actionPreview, ""))
	if out.success || out.detail != "Build failed before compilation completed" || !strings.HasPrefix(out.log, "failed to launch pdflatex: ") {
		t.Fatalf("outcome %+v", out)
	}
}

func TestExportTargets(t *testing.T) {
	isolate(t)
	setupStandins(t, "", "pdflatex")
	root, file := newDocument(t)

	target := defaultExportPDFPath(root, file)
	if target != filepath.Join(root, "chapters", "My Thesis.pdf") {
		t.Fatalf("default export %s", target)
	}
	out := compileDocument(buildPlanFor(1, root, file, "x", actionExportDefault, target))
	if out.exportedPDFPath != target || out.detail != "pdflatex finished successfully and exported "+target {
		t.Fatalf("outcome %+v", out)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal(err)
	}

	if got := defaultExportPDFPath(root, ""); got != filepath.Join(root, "main.pdf") {
		t.Fatalf("untitled export %s", got)
	}
	untitled := buildPlanFor(2, root, "", "x", actionPreview, "")
	if untitled.sourceDir != root || untitled.jobName != "main" || filepath.Base(untitled.tempSourcePath) != "main.tex" {
		t.Fatalf("untitled plan %+v", untitled)
	}

	chosen := filepath.Join(t.TempDir(), "new", "folder", "out.pdf")
	out = compileDocument(buildPlanFor(3, root, file, "x", actionExportTo, chosen))
	if out.exportedPDFPath != chosen {
		t.Fatalf("outcome %+v", out)
	}
}

func TestPruning(t *testing.T) {
	dir := t.TempDir()
	for i := range 20 {
		writeFile(t, filepath.Join(dir, fmt.Sprintf("%02d.tex", i)), "x")
		mt := fixedTime(i)
		os.Chtimes(filepath.Join(dir, fmt.Sprintf("%02d.tex", i)), mt, mt)
	}
	if err := pruneOldFiles(dir, historyLimit); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != historyLimit || entries[0].Name() != "04.tex" {
		t.Fatalf("kept %d, first %s", len(entries), entries[0].Name())
	}

	parent := t.TempDir()
	for i := range 30 {
		p := filepath.Join(parent, fmt.Sprintf("doc%02d", i))
		os.Mkdir(p, 0o755)
		os.Chtimes(p, fixedTime(i), fixedTime(i))
	}
	if err := pruneOldDirectories(parent, documentCacheLimit); err != nil {
		t.Fatal(err)
	}
	entries, _ = os.ReadDir(parent)
	if len(entries) != documentCacheLimit || entries[0].Name() != "doc06" {
		t.Fatalf("kept %d, first %s", len(entries), entries[0].Name())
	}
}
