#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

use std::{
    borrow::Cow,
    collections::hash_map::DefaultHasher,
    ffi::OsStr,
    fs,
    hash::{Hash, Hasher},
    path::{Path, PathBuf},
    process::{Command, Output},
    sync::{Arc, Mutex},
    time::{SystemTime, UNIX_EPOCH},
};

use anyhow::{Context, Result, anyhow};
use rfd::FileDialog;
use serde::{Deserialize, Serialize};
use tao::{
    dpi::LogicalSize,
    event::{Event, WindowEvent},
    event_loop::{ControlFlow, EventLoopBuilder, EventLoopProxy},
    window::WindowBuilder,
};
use wry::{
    WebViewBuilder,
    http::{Request, Response, StatusCode, header::CONTENT_TYPE},
};

const APP_URL: &str = "overustex://app/index.html";
const UI_HTML: &str = include_str!("ui.html");
const APP_STORAGE_DIR_NAME: &str = "OverusTeX";
const LEGACY_WORKSPACE_DIR_NAME: &str = ".overustex";
const HISTORY_LIMIT: usize = 16;
const PREVIEW_LIMIT: usize = 6;
const DOCUMENT_CACHE_LIMIT: usize = 24;
#[cfg(target_os = "windows")]
const CREATE_NO_WINDOW: u32 = 0x0800_0000;

#[cfg(target_os = "windows")]
use std::os::windows::process::CommandExt;
const DEFAULT_TEMPLATE: &str = r#"\documentclass{article}
\usepackage[utf8]{inputenc}
\usepackage[T1]{fontenc}

\title{OverusTeX}
\author{}
\date{\today}

\begin{document}
\maketitle

Hello from OverusTeX.

\end{document}
"#;

struct AppState {
    current_file: Option<PathBuf>,
    workspace_root: PathBuf,
    active_build_id: u64,
}

impl Default for AppState {
    fn default() -> Self {
        Self {
            current_file: None,
            workspace_root: default_workspace_root(),
            active_build_id: 0,
        }
    }
}

#[derive(Default)]
struct ProtocolState {
    preview_pdf: Option<PathBuf>,
}

#[derive(Clone, Serialize)]
struct WorkspaceEntry {
    name: String,
    relative_path: String,
    is_dir: bool,
    kind: String,
    children: Vec<WorkspaceEntry>,
}

#[derive(Deserialize)]
#[serde(tag = "cmd", rename_all = "snake_case")]
enum IpcCommand {
    Ready,
    NewDocument,
    OpenFile,
    OpenFolder,
    OpenWorkspaceFile { path: String },
    Save { contents: String },
    SaveAs { contents: String },
    Run { contents: String },
    ExportPdf { contents: String },
    SavePdfAs { contents: String },
}

#[derive(Serialize)]
#[serde(tag = "type", rename_all = "snake_case")]
enum FrontendEvent {
    DocumentLoaded {
        file_name: String,
        display_path: String,
        contents: String,
        preview_url: Option<String>,
        preview_label: String,
        note: String,
        workspace_root: String,
        workspace_entries: Vec<WorkspaceEntry>,
    },
    DocumentSaved {
        file_name: String,
        display_path: String,
        note: String,
        preview_url: Option<String>,
        preview_label: String,
        workspace_root: String,
        workspace_entries: Vec<WorkspaceEntry>,
    },
    BuildStarted {
        detail: String,
        log: String,
    },
    BuildFinished {
        success: bool,
        detail: String,
        log: String,
        preview_url: Option<String>,
        preview_label: String,
        exported_pdf_path: Option<String>,
        workspace_root: String,
        workspace_entries: Vec<WorkspaceEntry>,
    },
    Status {
        message: String,
        detail: String,
        log: String,
    },
}

enum UserEvent {
    Frontend(FrontendEvent),
    BuildFinished(BuildOutcome),
}

struct BuildOutcome {
    build_id: u64,
    success: bool,
    detail: String,
    log: String,
    preview_pdf: Option<PathBuf>,
    exported_pdf_path: Option<PathBuf>,
}

enum BuildAction {
    Preview,
    ExportDefault(PathBuf),
    ExportTo(PathBuf),
}

struct BuildPlan {
    build_id: u64,
    source_dir: PathBuf,
    cache_dir: PathBuf,
    temp_source_path: PathBuf,
    source_file_name: String,
    job_name: String,
    contents: String,
    action: BuildAction,
}

fn main() -> Result<()> {
    let app_state = Arc::new(Mutex::new(AppState::default()));
    let protocol_state = Arc::new(Mutex::new(ProtocolState::default()));

    let event_loop = EventLoopBuilder::<UserEvent>::with_user_event().build();
    let proxy = event_loop.create_proxy();

    let window = WindowBuilder::new()
        .with_title("OverusTeX")
        .with_inner_size(LogicalSize::new(1440.0, 920.0))
        .with_min_inner_size(LogicalSize::new(960.0, 680.0))
        .build(&event_loop)
        .context("failed to create window")?;

    let protocol_state_for_protocol = Arc::clone(&protocol_state);
    let app_state_for_ipc = Arc::clone(&app_state);
    let protocol_state_for_ipc = Arc::clone(&protocol_state);
    let proxy_for_ipc = proxy.clone();

    let webview = WebViewBuilder::new()
        .with_custom_protocol("overustex".into(), move |_id, request: Request<Vec<u8>>| {
            let path = request.uri().path();
            match path {
                "/" | "/index.html" => ok_response("text/html; charset=utf-8", UI_HTML.as_bytes()),
                "/preview.pdf" => serve_preview_pdf(&protocol_state_for_protocol, &request),
                _ => not_found_response("Not found"),
            }
        })
        .with_ipc_handler(move |message: Request<String>| {
            if let Err(error) = handle_ipc(
                &proxy_for_ipc,
                &app_state_for_ipc,
                &protocol_state_for_ipc,
                message.body().to_string(),
            ) {
                emit(
                    &proxy_for_ipc,
                    FrontendEvent::Status {
                        message: "Something went wrong".to_string(),
                        detail: error.to_string(),
                        log: format!("{error:#}"),
                    },
                );
            }
        })
        .with_url(APP_URL)
        .build(&window)
        .context("failed to build webview")?;

    event_loop.run(move |event, _, control_flow| {
        *control_flow = ControlFlow::Wait;

        match event {
            Event::WindowEvent {
                event: WindowEvent::CloseRequested,
                ..
            } => {
                *control_flow = ControlFlow::Exit;
            }
            Event::UserEvent(UserEvent::Frontend(frontend_event)) => {
                if let Err(error) = dispatch_to_frontend(&webview, &frontend_event) {
                    eprintln!("frontend dispatch error: {error:#}");
                }
            }
            Event::UserEvent(UserEvent::BuildFinished(outcome)) => {
                let (preview_url, workspace_root, workspace_items) =
                    if let Ok(state) = app_state.lock() {
                        let preview_url =
                            if state.active_build_id == outcome.build_id && outcome.success {
                                if let Some(pdf) = outcome.preview_pdf.as_ref() {
                                    if let Ok(mut preview_state) = protocol_state.lock() {
                                        preview_state.preview_pdf = Some(pdf.clone());
                                    }
                                    Some(preview_url())
                                } else {
                                    None
                                }
                            } else {
                                None
                            };
                        let workspace_root = state.workspace_root.display().to_string();
                        let workspace_items =
                            workspace_entries(&state.workspace_root).unwrap_or_default();
                        (preview_url, workspace_root, workspace_items)
                    } else {
                        (None, String::new(), Vec::new())
                    };

                let event = FrontendEvent::BuildFinished {
                    success: outcome.success,
                    detail: outcome.detail,
                    log: outcome.log,
                    preview_url,
                    preview_label: if outcome.success {
                        "rendered".to_string()
                    } else {
                        "build failed".to_string()
                    },
                    exported_pdf_path: outcome
                        .exported_pdf_path
                        .map(|path| path.display().to_string()),
                    workspace_root,
                    workspace_entries: workspace_items,
                };

                if let Err(error) = dispatch_to_frontend(&webview, &event) {
                    eprintln!("build event dispatch error: {error:#}");
                }
            }
            _ => {}
        }
    });
}

fn handle_ipc(
    proxy: &EventLoopProxy<UserEvent>,
    app_state: &Arc<Mutex<AppState>>,
    protocol_state: &Arc<Mutex<ProtocolState>>,
    message: String,
) -> Result<()> {
    let command: IpcCommand = serde_json::from_str(&message).context("invalid IPC payload")?;

    match command {
        IpcCommand::Ready => {
            let default_file = default_run_path();
            if default_file.exists() {
                let contents = fs::read_to_string(&default_file).with_context(|| {
                    format!("failed to read startup file {}", default_file.display())
                })?;
                let workspace_root = default_file
                    .parent()
                    .map(Path::to_path_buf)
                    .unwrap_or_else(default_workspace_root);
                {
                    let mut state = lock_app_state(app_state)?;
                    state.current_file = Some(default_file.clone());
                    state.workspace_root = workspace_root.clone();
                }
                {
                    let mut state = lock_protocol_state(protocol_state)?;
                    state.preview_pdf = None;
                }
                emit(
                    proxy,
                    FrontendEvent::DocumentLoaded {
                        file_name: file_name(Some(&default_file)),
                        display_path: display_path(Some(&default_file)),
                        contents,
                        preview_url: None,
                        preview_label: "waiting for build".to_string(),
                        note: format!(
                            "Loaded {}. Run builds an internal working copy without writing next to the source file.",
                            default_file.display()
                        ),
                        workspace_root: workspace_root.display().to_string(),
                        workspace_entries: workspace_entries(&workspace_root)?,
                    },
                );
            } else {
                let workspace_root = default_workspace_root();
                {
                    let mut state = lock_app_state(app_state)?;
                    state.current_file = None;
                    state.workspace_root = workspace_root.clone();
                }
                {
                    let mut state = lock_protocol_state(protocol_state)?;
                    state.preview_pdf = None;
                }
                emit(
                    proxy,
                    FrontendEvent::DocumentLoaded {
                        file_name: "untitled.tex".to_string(),
                        display_path: "Untitled".to_string(),
                        contents: DEFAULT_TEMPLATE.to_string(),
                        preview_url: None,
                        preview_label: "waiting for build".to_string(),
                        note: "New document ready. Press Run to build a preview without saving the .tex file.".to_string(),
                        workspace_root: workspace_root.display().to_string(),
                        workspace_entries: workspace_entries(&workspace_root)?,
                    },
                );
            }
        }
        IpcCommand::NewDocument => {
            let workspace_root = {
                let mut state = lock_app_state(app_state)?;
                state.current_file = None;
                state.workspace_root.clone()
            };
            {
                let mut state = lock_protocol_state(protocol_state)?;
                state.preview_pdf = None;
            }
            emit(
                proxy,
                FrontendEvent::DocumentLoaded {
                    file_name: "untitled.tex".to_string(),
                    display_path: "Untitled".to_string(),
                    contents: DEFAULT_TEMPLATE.to_string(),
                    preview_url: None,
                    preview_label: "waiting for build".to_string(),
                    note: "Started a fresh LaTeX buffer.".to_string(),
                    workspace_root: workspace_root.display().to_string(),
                    workspace_entries: workspace_entries(&workspace_root)?,
                },
            );
        }
        IpcCommand::OpenFolder => {
            let current_dir = lock_app_state(app_state)?.workspace_root.clone();
            if let Some(folder) = FileDialog::new().set_directory(current_dir).pick_folder() {
                let main_tex = folder.join("main.tex");
                if main_tex.exists() {
                    let contents = fs::read_to_string(&main_tex)
                        .with_context(|| format!("failed to read {}", main_tex.display()))?;
                    {
                        let mut state = lock_app_state(app_state)?;
                        state.current_file = Some(main_tex.clone());
                        state.workspace_root = folder.clone();
                    }
                    {
                        let mut state = lock_protocol_state(protocol_state)?;
                        state.preview_pdf = None;
                    }
                    emit(
                        proxy,
                        FrontendEvent::DocumentLoaded {
                            file_name: file_name(Some(&main_tex)),
                            display_path: display_path(Some(&main_tex)),
                            contents,
                            preview_url: None,
                            preview_label: "waiting for build".to_string(),
                            note: format!(
                                "Opened workspace {} and loaded main.tex.",
                                folder.display()
                            ),
                            workspace_root: folder.display().to_string(),
                            workspace_entries: workspace_entries(&folder)?,
                        },
                    );
                } else {
                    {
                        let mut state = lock_app_state(app_state)?;
                        state.current_file = None;
                        state.workspace_root = folder.clone();
                    }
                    {
                        let mut state = lock_protocol_state(protocol_state)?;
                        state.preview_pdf = None;
                    }
                    emit(
                        proxy,
                        FrontendEvent::DocumentLoaded {
                            file_name: "untitled.tex".to_string(),
                            display_path: "Untitled".to_string(),
                            contents: DEFAULT_TEMPLATE.to_string(),
                            preview_url: None,
                            preview_label: "waiting for build".to_string(),
                            note: format!("Opened workspace {}.", folder.display()),
                            workspace_root: folder.display().to_string(),
                            workspace_entries: workspace_entries(&folder)?,
                        },
                    );
                }
            }
        }
        IpcCommand::OpenFile => {
            let current_root = lock_app_state(app_state)?.workspace_root.clone();
            if let Some(path) = FileDialog::new()
                .add_filter("LaTeX", &["tex"])
                .set_directory(&current_root)
                .pick_file()
            {
                let contents = fs::read_to_string(&path)
                    .with_context(|| format!("failed to read {}", path.display()))?;
                let workspace_root = workspace_root_for_path(&current_root, &path);
                {
                    let mut state = lock_app_state(app_state)?;
                    state.current_file = Some(path.clone());
                    state.workspace_root = workspace_root.clone();
                }
                {
                    let mut state = lock_protocol_state(protocol_state)?;
                    state.preview_pdf = None;
                }
                emit(
                    proxy,
                    FrontendEvent::DocumentLoaded {
                        file_name: file_name(Some(&path)),
                        display_path: display_path(Some(&path)),
                        contents,
                        preview_url: None,
                        preview_label: "waiting for build".to_string(),
                        note: format!("Opened {}.", path.display()),
                        workspace_root: workspace_root.display().to_string(),
                        workspace_entries: workspace_entries(&workspace_root)?,
                    },
                );
            }
        }
        IpcCommand::OpenWorkspaceFile { path } => {
            let current_root = lock_app_state(app_state)?.workspace_root.clone();
            let path = {
                let candidate = PathBuf::from(&path);
                if candidate.is_absolute() {
                    candidate
                } else {
                    current_root.join(candidate)
                }
            };
            let contents = fs::read_to_string(&path)
                .with_context(|| format!("failed to read {}", path.display()))?;
            let workspace_root = workspace_root_for_path(&current_root, &path);
            {
                let mut state = lock_app_state(app_state)?;
                state.current_file = Some(path.clone());
                state.workspace_root = workspace_root.clone();
            }
            {
                let mut state = lock_protocol_state(protocol_state)?;
                state.preview_pdf = None;
            }
            emit(
                proxy,
                FrontendEvent::DocumentLoaded {
                    file_name: file_name(Some(&path)),
                    display_path: display_path(Some(&path)),
                    contents,
                    preview_url: None,
                    preview_label: "waiting for build".to_string(),
                    note: format!("Opened {}.", path.display()),
                    workspace_root: workspace_root.display().to_string(),
                    workspace_entries: workspace_entries(&workspace_root)?,
                },
            );
        }
        IpcCommand::Save { contents } => {
            let (current_path, workspace_root) = {
                let state = lock_app_state(app_state)?;
                (state.current_file.clone(), state.workspace_root.clone())
            };
            let suggested_name = current_path
                .as_ref()
                .and_then(|path| path.file_name())
                .map(|name| name.to_string_lossy().to_string())
                .unwrap_or_else(|| "main.tex".to_string());
            let Some(path) =
                current_path.or_else(|| choose_save_path(&workspace_root, &suggested_name))
            else {
                return Ok(());
            };

            save_document(&path, &contents)?;
            let workspace_root = workspace_root_for_path(&workspace_root, &path);
            {
                let mut state = lock_app_state(app_state)?;
                state.current_file = Some(path.clone());
                state.workspace_root = workspace_root.clone();
            }
            let (preview_url, preview_label) = {
                let state = lock_protocol_state(protocol_state)?;
                (
                    state.preview_pdf.as_ref().map(|_| preview_url()),
                    if state.preview_pdf.is_some() {
                        "rendered".to_string()
                    } else {
                        "waiting for build".to_string()
                    },
                )
            };
            emit(
                proxy,
                FrontendEvent::DocumentSaved {
                    file_name: file_name(Some(&path)),
                    display_path: display_path(Some(&path)),
                    note: format!("Saved {}.", path.display()),
                    preview_url,
                    preview_label,
                    workspace_root: workspace_root.display().to_string(),
                    workspace_entries: workspace_entries(&workspace_root)?,
                },
            );
        }
        IpcCommand::SaveAs { contents } => {
            let (workspace_root, suggested_name) = {
                let state = lock_app_state(app_state)?;
                (
                    state.workspace_root.clone(),
                    state
                        .current_file
                        .as_ref()
                        .and_then(|path| path.file_name())
                        .map(|name| name.to_string_lossy().to_string())
                        .unwrap_or_else(|| "main.tex".to_string()),
                )
            };
            let Some(path) = choose_save_path(&workspace_root, &suggested_name) else {
                return Ok(());
            };

            save_document(&path, &contents)?;
            let workspace_root = workspace_root_for_path(&workspace_root, &path);
            {
                let mut state = lock_app_state(app_state)?;
                state.current_file = Some(path.clone());
                state.workspace_root = workspace_root.clone();
            }
            let (preview_url, preview_label) = {
                let state = lock_protocol_state(protocol_state)?;
                (
                    state.preview_pdf.as_ref().map(|_| preview_url()),
                    if state.preview_pdf.is_some() {
                        "rendered".to_string()
                    } else {
                        "waiting for build".to_string()
                    },
                )
            };
            emit(
                proxy,
                FrontendEvent::DocumentSaved {
                    file_name: file_name(Some(&path)),
                    display_path: display_path(Some(&path)),
                    note: format!("Saved {}.", path.display()),
                    preview_url,
                    preview_label,
                    workspace_root: workspace_root.display().to_string(),
                    workspace_entries: workspace_entries(&workspace_root)?,
                },
            );
        }
        IpcCommand::Run { contents } => {
            let build_plan = {
                let mut state = lock_app_state(app_state)?;
                state.active_build_id += 1;
                build_plan_for(
                    state.active_build_id,
                    &state.workspace_root,
                    state.current_file.as_deref(),
                    contents,
                    BuildAction::Preview,
                )
            };
            emit(
                proxy,
                FrontendEvent::BuildStarted {
                    detail: "Preview build in progress".to_string(),
                    log: format!(
                        "Building {} as an internal working copy ...",
                        build_plan.source_file_name
                    ),
                },
            );

            let proxy = proxy.clone();
            std::thread::spawn(move || {
                let outcome = compile_document(build_plan);
                let _ = proxy.send_event(UserEvent::BuildFinished(outcome));
            });
        }
        IpcCommand::ExportPdf { contents } => {
            let build_plan = {
                let mut state = lock_app_state(app_state)?;
                state.active_build_id += 1;
                let export_target =
                    default_export_pdf_path(&state.workspace_root, state.current_file.as_deref());
                build_plan_for(
                    state.active_build_id,
                    &state.workspace_root,
                    state.current_file.as_deref(),
                    contents,
                    BuildAction::ExportDefault(export_target),
                )
            };
            emit(
                proxy,
                FrontendEvent::BuildStarted {
                    detail: "Exporting PDF".to_string(),
                    log: "Building the current buffer and exporting a PDF copy...".to_string(),
                },
            );

            let proxy = proxy.clone();
            std::thread::spawn(move || {
                let outcome = compile_document(build_plan);
                let _ = proxy.send_event(UserEvent::BuildFinished(outcome));
            });
        }
        IpcCommand::SavePdfAs { contents } => {
            let workspace_root = lock_app_state(app_state)?.workspace_root.clone();
            let Some(target) = choose_pdf_save_path(&workspace_root, "export.pdf") else {
                return Ok(());
            };
            let build_plan = {
                let mut state = lock_app_state(app_state)?;
                state.active_build_id += 1;
                build_plan_for(
                    state.active_build_id,
                    &state.workspace_root,
                    state.current_file.as_deref(),
                    contents,
                    BuildAction::ExportTo(target),
                )
            };
            emit(
                proxy,
                FrontendEvent::BuildStarted {
                    detail: "Exporting PDF".to_string(),
                    log:
                        "Building the current buffer and saving the PDF to your chosen location..."
                            .to_string(),
                },
            );

            let proxy = proxy.clone();
            std::thread::spawn(move || {
                let outcome = compile_document(build_plan);
                let _ = proxy.send_event(UserEvent::BuildFinished(outcome));
            });
        }
    }

    Ok(())
}

fn compile_document(plan: BuildPlan) -> BuildOutcome {
    let error_build_id = plan.build_id;
    let build = (|| -> Result<BuildOutcome> {
        fs::create_dir_all(document_cache_parent())
            .with_context(|| format!("failed to create {}", document_cache_parent().display()))?;
        prune_old_directories(&document_cache_parent(), DOCUMENT_CACHE_LIMIT)?;
        fs::create_dir_all(&plan.cache_dir)
            .with_context(|| format!("failed to create {}", plan.cache_dir.display()))?;
        fs::create_dir_all(build_output_dir(&plan.cache_dir)).with_context(|| {
            format!(
                "failed to create {}",
                build_output_dir(&plan.cache_dir).display()
            )
        })?;
        fs::create_dir_all(preview_output_dir(&plan.cache_dir)).with_context(|| {
            format!(
                "failed to create {}",
                preview_output_dir(&plan.cache_dir).display()
            )
        })?;

        write_snapshot(&plan.cache_dir, &plan.job_name, &plan.contents)?;

        save_document(&plan.temp_source_path, &plan.contents)?;

        let (detail, output, used_engine) = if can_use_latexmk() {
            let mut command = Command::new("latexmk");
            command
                .current_dir(&plan.source_dir)
                .arg("-pdf")
                .arg("-interaction=nonstopmode")
                .arg("-synctex=1")
                .arg("-file-line-error")
                .arg(format!(
                    "-outdir={}",
                    build_output_dir(&plan.cache_dir).display()
                ))
                .arg(format!("-jobname={}", plan.job_name))
                .arg(&plan.temp_source_path);
            apply_tex_inputs(&mut command, &plan.source_dir);
            match run_command_capture_hidden(&mut command) {
                Ok(output) => ("latexmk".to_string(), output, "latexmk".to_string()),
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                    let output = run_pdflatex_twice(
                        &plan.source_dir,
                        &plan.cache_dir,
                        &plan.temp_source_path,
                        &plan.job_name,
                    )?;
                    ("pdflatex".to_string(), output, "pdflatex".to_string())
                }
                Err(error) => {
                    return Err(error).context("failed to launch latexmk");
                }
            }
        } else {
            let output = run_pdflatex_twice(
                &plan.source_dir,
                &plan.cache_dir,
                &plan.temp_source_path,
                &plan.job_name,
            )?;
            ("pdflatex".to_string(), output, "pdflatex".to_string())
        };

        let _ = fs::remove_file(&plan.temp_source_path);

        let log = format_command_output(&used_engine, &output);
        let built_pdf = build_output_dir(&plan.cache_dir).join(format!("{}.pdf", plan.job_name));

        if output.status.success() && built_pdf.exists() {
            let preview_pdf = create_preview_snapshot(&plan.cache_dir, &plan.job_name, &built_pdf)?;
            let exported_pdf_path = match &plan.action {
                BuildAction::Preview => None,
                BuildAction::ExportDefault(target) | BuildAction::ExportTo(target) => {
                    if let Some(parent) = target.parent() {
                        fs::create_dir_all(parent).with_context(|| {
                            format!("failed to create export directory {}", parent.display())
                        })?;
                    }
                    fs::copy(&built_pdf, target)
                        .with_context(|| format!("failed to export PDF to {}", target.display()))?;
                    Some(target.clone())
                }
            };
            Ok(BuildOutcome {
                build_id: plan.build_id,
                success: true,
                detail: match exported_pdf_path.as_ref() {
                    Some(path) => format!(
                        "{detail} finished successfully and exported {}",
                        path.display()
                    ),
                    None => format!("{detail} finished successfully"),
                },
                log,
                preview_pdf: Some(preview_pdf),
                exported_pdf_path,
            })
        } else {
            Ok(BuildOutcome {
                build_id: plan.build_id,
                success: false,
                detail: format!("{detail} reported an error"),
                log,
                preview_pdf: None,
                exported_pdf_path: None,
            })
        }
    })();

    match build {
        Ok(outcome) => outcome,
        Err(error) => BuildOutcome {
            build_id: error_build_id,
            success: false,
            detail: "Build failed before compilation completed".to_string(),
            log: format!("{error:#}"),
            preview_pdf: None,
            exported_pdf_path: None,
        },
    }
}

fn can_use_latexmk() -> bool {
    Command::new("perl")
        .arg("-v")
        .output()
        .map(|output| output.status.success())
        .unwrap_or(false)
}

fn run_pdflatex_twice(
    source_dir: &Path,
    cache_dir: &Path,
    source_path: &Path,
    job_name: &str,
) -> Result<Output> {
    let mut first_command = Command::new("pdflatex");
    first_command
        .current_dir(source_dir)
        .arg("-interaction=nonstopmode")
        .arg("-synctex=1")
        .arg("-file-line-error")
        .arg(format!(
            "-output-directory={}",
            build_output_dir(cache_dir).display()
        ))
        .arg(format!("-jobname={job_name}"))
        .arg(source_path);
    apply_tex_inputs(&mut first_command, source_dir);
    let first =
        run_command_capture_hidden(&mut first_command).context("failed to launch pdflatex")?;

    if !first.status.success() {
        return Ok(first);
    }

    let mut second_command = Command::new("pdflatex");
    second_command
        .current_dir(source_dir)
        .arg("-interaction=nonstopmode")
        .arg("-synctex=1")
        .arg("-file-line-error")
        .arg(format!(
            "-output-directory={}",
            build_output_dir(cache_dir).display()
        ))
        .arg(format!("-jobname={job_name}"))
        .arg(source_path);
    apply_tex_inputs(&mut second_command, source_dir);
    let second =
        run_command_capture_hidden(&mut second_command).context("failed to launch pdflatex")?;

    Ok(merge_outputs(first, second))
}

fn save_document(path: &Path, contents: &str) -> Result<()> {
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)
            .with_context(|| format!("failed to create {}", parent.display()))?;
    }
    fs::write(path, contents).with_context(|| format!("failed to write {}", path.display()))
}

fn choose_save_path(base_dir: &Path, suggested_file_name: &str) -> Option<PathBuf> {
    FileDialog::new()
        .add_filter("LaTeX", &["tex"])
        .set_directory(base_dir)
        .set_file_name(suggested_file_name)
        .save_file()
}

fn choose_pdf_save_path(base_dir: &Path, suggested_file_name: &str) -> Option<PathBuf> {
    FileDialog::new()
        .add_filter("PDF", &["pdf"])
        .set_directory(base_dir)
        .set_file_name(suggested_file_name)
        .save_file()
}

fn build_plan_for(
    build_id: u64,
    workspace_root: &Path,
    current_file: Option<&Path>,
    contents: String,
    action: BuildAction,
) -> BuildPlan {
    let source_file_name = current_file
        .and_then(Path::file_name)
        .map(|name| name.to_string_lossy().to_string())
        .unwrap_or_else(|| "main.tex".to_string());
    let stem = current_file
        .and_then(Path::file_stem)
        .map(|name| name.to_string_lossy().to_string())
        .unwrap_or_else(|| "main".to_string());
    let source_dir = current_file
        .and_then(Path::parent)
        .map(Path::to_path_buf)
        .unwrap_or_else(|| workspace_root.to_path_buf());
    let cache_dir = document_cache_dir(workspace_root, current_file);
    let temp_source_path = preview_source_path(&cache_dir, &source_file_name);

    BuildPlan {
        build_id,
        source_dir,
        cache_dir,
        temp_source_path,
        source_file_name,
        job_name: sanitize_job_name(&stem),
        contents,
        action,
    }
}

fn default_export_pdf_path(workspace_root: &Path, current_file: Option<&Path>) -> PathBuf {
    current_file
        .map(|path| path.with_extension("pdf"))
        .unwrap_or_else(|| workspace_root.join("main.pdf"))
}

fn app_cache_dir() -> PathBuf {
    dirs::cache_dir()
        .or_else(dirs::data_local_dir)
        .unwrap_or_else(std::env::temp_dir)
        .join(APP_STORAGE_DIR_NAME)
}

fn document_cache_parent() -> PathBuf {
    app_cache_dir().join("documents")
}

fn document_cache_dir(workspace_root: &Path, current_file: Option<&Path>) -> PathBuf {
    let identity = current_file
        .map(|path| format!("file:{}", path.display()))
        .unwrap_or_else(|| format!("untitled:{}", workspace_root.display()));
    let label = current_file
        .and_then(Path::file_stem)
        .map(|name| name.to_string_lossy().to_string())
        .or_else(|| {
            workspace_root
                .file_name()
                .map(|name| name.to_string_lossy().to_string())
        })
        .unwrap_or_else(|| "untitled".to_string());
    let mut hasher = DefaultHasher::new();
    identity.hash(&mut hasher);
    let digest = hasher.finish();
    document_cache_parent().join(format!("{}-{digest:016x}", sanitize_job_name(&label)))
}

fn build_output_dir(cache_dir: &Path) -> PathBuf {
    cache_dir.join("build")
}

fn preview_output_dir(cache_dir: &Path) -> PathBuf {
    cache_dir.join("preview")
}

fn history_dir(cache_dir: &Path) -> PathBuf {
    cache_dir.join("history")
}

fn preview_source_path(cache_dir: &Path, source_file_name: &str) -> PathBuf {
    cache_dir.join("source").join(source_file_name)
}

fn sanitize_job_name(value: &str) -> String {
    let sanitized: String = value
        .chars()
        .map(|character| {
            if character.is_ascii_alphanumeric() || character == '-' || character == '_' {
                character
            } else {
                '_'
            }
        })
        .collect();

    if sanitized.is_empty() {
        "main".to_string()
    } else {
        sanitized
    }
}

fn write_snapshot(cache_dir: &Path, job_name: &str, contents: &str) -> Result<()> {
    let history_dir = history_dir(cache_dir);
    fs::create_dir_all(&history_dir)
        .with_context(|| format!("failed to create {}", history_dir.display()))?;
    let snapshot_name = format!("{:020}-{}.tex", unix_timestamp_millis(), job_name);
    save_document(&history_dir.join(snapshot_name), contents)?;
    prune_old_files(&history_dir, HISTORY_LIMIT)?;
    Ok(())
}

fn create_preview_snapshot(cache_dir: &Path, job_name: &str, built_pdf: &Path) -> Result<PathBuf> {
    let preview_dir = preview_output_dir(cache_dir);
    fs::create_dir_all(&preview_dir)
        .with_context(|| format!("failed to create {}", preview_dir.display()))?;
    let preview_path = preview_dir.join(format!("{job_name}-{}.pdf", unix_timestamp_millis()));
    fs::copy(built_pdf, &preview_path).with_context(|| {
        format!(
            "failed to create preview snapshot {} from {}",
            preview_path.display(),
            built_pdf.display()
        )
    })?;
    prune_old_files(&preview_dir, PREVIEW_LIMIT)?;
    Ok(preview_path)
}

fn prune_old_files(directory: &Path, keep: usize) -> Result<()> {
    let mut entries = fs::read_dir(directory)
        .with_context(|| format!("failed to read {}", directory.display()))?
        .filter_map(|entry| entry.ok())
        .collect::<Vec<_>>();
    entries.sort_by_key(|entry| {
        entry
            .metadata()
            .and_then(|metadata| metadata.modified())
            .ok()
    });
    if entries.len() <= keep {
        return Ok(());
    }

    let remove_count = entries.len() - keep;
    for entry in entries.into_iter().take(remove_count) {
        let path = entry.path();
        if path.is_file() {
            let _ = fs::remove_file(path);
        }
    }
    Ok(())
}

fn prune_old_directories(directory: &Path, keep: usize) -> Result<()> {
    if !directory.exists() {
        return Ok(());
    }

    let mut entries = fs::read_dir(directory)
        .with_context(|| format!("failed to read {}", directory.display()))?
        .filter_map(|entry| entry.ok())
        .filter(|entry| {
            entry
                .file_type()
                .map(|file_type| file_type.is_dir())
                .unwrap_or(false)
        })
        .collect::<Vec<_>>();
    entries.sort_by_key(|entry| {
        entry
            .metadata()
            .and_then(|metadata| metadata.modified())
            .ok()
    });
    if entries.len() <= keep {
        return Ok(());
    }

    let remove_count = entries.len() - keep;
    for entry in entries.into_iter().take(remove_count) {
        let _ = fs::remove_dir_all(entry.path());
    }
    Ok(())
}

fn workspace_entries(root: &Path) -> Result<Vec<WorkspaceEntry>> {
    if !root.exists() {
        return Ok(Vec::new());
    }
    workspace_entries_recursive(root, root, 0)
}

fn workspace_entries_recursive(
    root: &Path,
    current: &Path,
    depth: usize,
) -> Result<Vec<WorkspaceEntry>> {
    if depth > 6 {
        return Ok(Vec::new());
    }

    let mut entries = fs::read_dir(current)
        .with_context(|| format!("failed to read {}", current.display()))?
        .filter_map(|entry| entry.ok())
        .filter(|entry| {
            let name = entry.file_name();
            let name = name.to_string_lossy();
            name != LEGACY_WORKSPACE_DIR_NAME && name != ".git" && name != "target"
        })
        .collect::<Vec<_>>();

    entries.sort_by_key(|entry| {
        let is_dir = entry
            .file_type()
            .map(|file_type| file_type.is_dir())
            .unwrap_or(false);
        (
            !is_dir,
            entry.file_name().to_string_lossy().to_ascii_lowercase(),
        )
    });

    let mut items = Vec::new();
    for entry in entries {
        let path = entry.path();
        let file_type = entry.file_type()?;
        let is_dir = file_type.is_dir();
        let children = if is_dir {
            workspace_entries_recursive(root, &path, depth + 1)?
        } else {
            Vec::new()
        };
        items.push(WorkspaceEntry {
            name: entry.file_name().to_string_lossy().to_string(),
            relative_path: path
                .strip_prefix(root)
                .unwrap_or(&path)
                .to_string_lossy()
                .replace('\\', "/"),
            is_dir,
            kind: workspace_kind(&path, is_dir),
            children,
        });
    }

    Ok(items)
}

fn workspace_kind(path: &Path, is_dir: bool) -> String {
    if is_dir {
        return "dir".to_string();
    }

    match path
        .extension()
        .and_then(OsStr::to_str)
        .map(|extension| extension.to_ascii_lowercase())
        .as_deref()
    {
        Some("tex") => "tex".to_string(),
        Some("png") | Some("jpg") | Some("jpeg") | Some("gif") | Some("bmp") | Some("svg") => {
            "image".to_string()
        }
        Some("pdf") => "pdf".to_string(),
        _ => "file".to_string(),
    }
}

fn workspace_root_for_path(current_root: &Path, path: &Path) -> PathBuf {
    if path.starts_with(current_root) {
        current_root.to_path_buf()
    } else {
        path.parent()
            .map(Path::to_path_buf)
            .unwrap_or_else(default_workspace_root)
    }
}

fn apply_tex_inputs(command: &mut Command, source_dir: &Path) {
    prepend_tex_path_env(command, "TEXINPUTS", source_dir);
    prepend_tex_path_env(command, "BIBINPUTS", source_dir);
    prepend_tex_path_env(command, "BSTINPUTS", source_dir);
}

fn prepend_tex_path_env(command: &mut Command, key: &str, source_dir: &Path) {
    let separator = if cfg!(windows) { ';' } else { ':' };
    let existing = std::env::var_os(key)
        .map(|value| value.to_string_lossy().into_owned())
        .unwrap_or_default();
    let mut value = source_dir.display().to_string();
    value.push(separator);
    if !existing.is_empty() {
        value.push_str(&existing);
    }
    command.env(key, value);
}

fn run_command_capture_hidden(command: &mut Command) -> std::io::Result<Output> {
    #[cfg(target_os = "windows")]
    {
        command.creation_flags(CREATE_NO_WINDOW);
    }
    command.output()
}

fn default_workspace_root() -> PathBuf {
    dirs::document_dir()
        .or_else(|| std::env::current_dir().ok())
        .unwrap_or_else(|| PathBuf::from("."))
        .join(APP_STORAGE_DIR_NAME)
}

fn default_run_path() -> PathBuf {
    default_workspace_root().join("main.tex")
}

fn file_name(path: Option<&Path>) -> String {
    path.and_then(Path::file_name)
        .map(|name| name.to_string_lossy().to_string())
        .unwrap_or_else(|| "untitled.tex".to_string())
}

fn display_path(path: Option<&Path>) -> String {
    path.map(|value| value.display().to_string())
        .unwrap_or_else(|| "Untitled".to_string())
}

fn preview_url() -> String {
    format!("/preview.pdf?ts={}", unix_timestamp_millis())
}

fn unix_timestamp_millis() -> u128 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|duration| duration.as_millis())
        .unwrap_or_default()
}

fn dispatch_to_frontend(webview: &wry::WebView, event: &FrontendEvent) -> Result<()> {
    let payload = serde_json::to_string(event).context("failed to serialize frontend event")?;
    webview
        .evaluate_script(&format!("window.overustexReceive({payload});"))
        .context("failed to evaluate frontend script")?;
    Ok(())
}

fn emit(proxy: &EventLoopProxy<UserEvent>, event: FrontendEvent) {
    let _ = proxy.send_event(UserEvent::Frontend(event));
}

fn lock_app_state(state: &Arc<Mutex<AppState>>) -> Result<std::sync::MutexGuard<'_, AppState>> {
    state.lock().map_err(|_| anyhow!("app state lock poisoned"))
}

fn lock_protocol_state(
    state: &Arc<Mutex<ProtocolState>>,
) -> Result<std::sync::MutexGuard<'_, ProtocolState>> {
    state
        .lock()
        .map_err(|_| anyhow!("preview state lock poisoned"))
}

fn format_command_output(label: &str, output: &Output) -> String {
    let mut sections = vec![format!("$ {label}")];

    let stdout = normalize_output(&output.stdout);
    if !stdout.is_empty() {
        sections.push(stdout);
    }

    let stderr = normalize_output(&output.stderr);
    if !stderr.is_empty() {
        sections.push(format!("[stderr]\n{stderr}"));
    }

    if !output.status.success() {
        sections.push(format!("[exit status] {}", output.status));
    }

    sections.join("\n\n")
}

fn normalize_output(bytes: &[u8]) -> String {
    String::from_utf8_lossy(bytes)
        .replace("\r\n", "\n")
        .trim()
        .to_string()
}

fn merge_outputs(first: Output, second: Output) -> Output {
    let mut stdout = first.stdout;
    stdout.extend_from_slice(b"\n\n");
    stdout.extend_from_slice(&second.stdout);

    let mut stderr = first.stderr;
    stderr.extend_from_slice(b"\n\n");
    stderr.extend_from_slice(&second.stderr);

    Output {
        status: second.status,
        stdout,
        stderr,
    }
}

fn serve_preview_pdf(
    protocol_state: &Arc<Mutex<ProtocolState>>,
    request: &Request<Vec<u8>>,
) -> Response<Cow<'static, [u8]>> {
    let pdf_path = match protocol_state.lock() {
        Ok(state) => match state.preview_pdf.as_ref() {
            Some(path) => path.clone(),
            None => return not_found_response("Preview PDF unavailable"),
        },
        Err(_) => return internal_error_response("Preview state lock poisoned"),
    };

    if !pdf_path.exists() {
        return not_found_response("Preview PDF not found");
    }

    let bytes = match fs::read(&pdf_path) {
        Ok(bytes) => bytes,
        Err(error) => {
            return internal_error_response(&format!(
                "failed to read {}: {error}",
                pdf_path.display()
            ));
        }
    };
    let total_len = bytes.len();

    if let Some(range_header) = request
        .headers()
        .get("range")
        .and_then(|value| value.to_str().ok())
    {
        if let Some((start, end)) = parse_range_header(range_header, total_len) {
            let body = bytes[start..=end].to_vec();
            return Response::builder()
                .status(StatusCode::PARTIAL_CONTENT)
                .header(CONTENT_TYPE, "application/pdf")
                .header("accept-ranges", "bytes")
                .header("cache-control", "no-store")
                .header("content-disposition", "inline; filename=\"preview.pdf\"")
                .header("content-length", body.len().to_string())
                .header("content-range", format!("bytes {start}-{end}/{total_len}"))
                .body(Cow::Owned(body))
                .unwrap_or_else(|_| internal_error_response("failed to build preview response"));
        }
        return Response::builder()
            .status(StatusCode::RANGE_NOT_SATISFIABLE)
            .header(CONTENT_TYPE, "text/plain; charset=utf-8")
            .header("content-range", format!("bytes */{total_len}"))
            .body(Cow::Owned(b"invalid range".to_vec()))
            .unwrap_or_else(|_| internal_error_response("failed to build range response"));
    }

    Response::builder()
        .status(StatusCode::OK)
        .header(CONTENT_TYPE, "application/pdf")
        .header("accept-ranges", "bytes")
        .header("cache-control", "no-store")
        .header("content-disposition", "inline; filename=\"preview.pdf\"")
        .header("content-length", total_len.to_string())
        .body(Cow::Owned(bytes))
        .unwrap_or_else(|_| internal_error_response("failed to build preview response"))
}

fn parse_range_header(range_header: &str, total_len: usize) -> Option<(usize, usize)> {
    let raw = range_header.strip_prefix("bytes=")?;
    let (start, end) = raw.split_once('-')?;

    if start.is_empty() {
        let suffix_len = end.parse::<usize>().ok()?;
        if suffix_len == 0 {
            return None;
        }
        let start = total_len.saturating_sub(suffix_len);
        let end = total_len.checked_sub(1)?;
        return Some((start, end));
    }

    let start = start.parse::<usize>().ok()?;
    let end = if end.is_empty() {
        total_len.checked_sub(1)?
    } else {
        end.parse::<usize>().ok()?
    };

    if start > end || end >= total_len {
        return None;
    }

    Some((start, end))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn nested_file_keeps_existing_workspace_root() {
        let workspace_root = PathBuf::from("/tmp/project");
        let nested_file = workspace_root.join("chapters/intro/main.tex");
        assert_eq!(
            workspace_root_for_path(&workspace_root, &nested_file),
            workspace_root
        );
    }

    #[test]
    fn external_file_switches_workspace_root_to_its_parent() {
        let workspace_root = PathBuf::from("/tmp/project");
        let external_file = PathBuf::from("/tmp/other/main.tex");
        assert_eq!(
            workspace_root_for_path(&workspace_root, &external_file),
            PathBuf::from("/tmp/other")
        );
    }

    #[test]
    fn missing_workspace_root_renders_as_empty_tree() {
        let missing =
            std::env::temp_dir().join(format!("overustex-missing-{}", unix_timestamp_millis()));
        assert!(workspace_entries(&missing).unwrap().is_empty());
    }

    #[test]
    fn document_cache_dir_is_stable_per_document_identity() {
        let workspace_root = PathBuf::from("/tmp/project");
        let first = document_cache_dir(&workspace_root, Some(Path::new("/tmp/project/main.tex")));
        let second = document_cache_dir(&workspace_root, Some(Path::new("/tmp/project/notes.tex")));
        assert_ne!(first, second);
        assert_eq!(
            first,
            document_cache_dir(&workspace_root, Some(Path::new("/tmp/project/main.tex")))
        );
    }
}

fn ok_response(content_type: &str, body: &[u8]) -> Response<Cow<'static, [u8]>> {
    response_with_status(StatusCode::OK, content_type, Cow::Owned(body.to_vec()))
}

fn not_found_response(message: &str) -> Response<Cow<'static, [u8]>> {
    response_with_status(
        StatusCode::NOT_FOUND,
        "text/plain; charset=utf-8",
        Cow::Owned(message.as_bytes().to_vec()),
    )
}

fn internal_error_response(message: &str) -> Response<Cow<'static, [u8]>> {
    response_with_status(
        StatusCode::INTERNAL_SERVER_ERROR,
        "text/plain; charset=utf-8",
        Cow::Owned(message.as_bytes().to_vec()),
    )
}

fn response_with_status(
    status: StatusCode,
    content_type: &str,
    body: Cow<'static, [u8]>,
) -> Response<Cow<'static, [u8]>> {
    Response::builder()
        .status(status)
        .header(CONTENT_TYPE, content_type)
        .body(body)
        .unwrap_or_else(|_| {
            Response::builder()
                .status(StatusCode::INTERNAL_SERVER_ERROR)
                .header(CONTENT_TYPE, "text/plain; charset=utf-8")
                .body(Cow::Borrowed(&b"response build error"[..]))
                .expect("static fallback response should build")
        })
}
