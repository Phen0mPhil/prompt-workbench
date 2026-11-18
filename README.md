# Prompt & Note Workbench

A lightweight, offline-first desktop note and prompt comparison tool built with a single-binary Go backend and an embedded zero-dependency HTML/JS frontend. Designed for large displays where viewing, cross-referencing, and editing up to three full-height documents side-by-side is essential.

## Core Features

- **Dual View Architecture:**
  - **Masonry Library:** Fast visual triage with global search across titles, body text, and tags.
  - **3-Pane Comparison Workspace:** Stage up to 3 notes and view them in full-height, side-by-side editable columns with active-pane focus borders.
- **Pointer & Literal Persistence:**
  - Notes created in the app persist automatically to `workbench_state.json` and save literal `.txt` copies to `./notes/` on close.
  - **Link File** attaches external `.txt`, `.html`, `.md`, `.docx`, or `.pdf` files by directory path pointer without duplicating large files, with one-click disk reload.
- **In-Pane Tooling:**
  - Word-style per-pane search (`Ctrl+F`) with live match counters and previous/next jump navigation.
  - Rich-text formatting (`Bold`, `Italic`, `Uppercase`, font size presets), ROYGBIV + White card tints, tag pill builder, and a recoverable Trash bin.

## Running & Building

Run locally (Linux / Windows):

    go run .

Cross-compile standalone Windows `.exe` from Linux:

    env GOOS=windows GOARCH=amd64 go build -ldflags="-H windowsgui" -o PromptWorkbench.exe
