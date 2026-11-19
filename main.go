package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

//go:embed frontend.html
var indexHTML []byte

var (
	baseDir       string
	docsDir       string
	junkDir       string
	stateFile     string
	stateMu       sync.Mutex
	ollamaCmd     *exec.Cmd
	weStartedOll  bool
	activeClients int32
)

type Note struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Content    string   `json:"content"`
	Color      string   `json:"color"`
	Tags       []string `json:"tags"`
	Trashed    bool     `json:"trashed"`
	SourceName string   `json:"sourceName,omitempty"`
	UpdatedAt  int64    `json:"updatedAt"`
}

type WorkbenchState struct {
	Notes        []Note   `json:"notes"`
	WorkspaceIDs []string `json:"workspaceIds"`
}

func sanitizeFilename(name string) string {
	clean := strings.TrimSpace(name)
	if clean == "" {
		clean = "untitled"
	}
	for _, bad := range []string{"\\", "/", ":", "*", "?", "\"", "<", ">", "|"} {
		clean = strings.ReplaceAll(clean, bad, "_")
	}
	return clean
}

func findOllamaBinary() string {
	if p, err := exec.LookPath("ollama"); err == nil {
		return p
	}
	if runtime.GOOS == "windows" {
		localApp := os.Getenv("LOCALAPPDATA")
		if localApp != "" {
			candidate := filepath.Join(localApp, "Programs", "Ollama", "ollama.exe")
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}
	return "ollama"
}

func isOllamaAlive() bool {
	client := &http.Client{Timeout: 800 * time.Millisecond}
	resp, err := client.Get("http://127.0.0.1:11434/api/tags")
	if err == nil {
		resp.Body.Close()
		return true
	}
	return false
}

func ensureOllamaRunning() bool {
	if isOllamaAlive() {
		return true
	}
	bin := findOllamaBinary()
	cmd := exec.Command(bin, "serve")
	cmd.Env = append(os.Environ(), "OLLAMA_VULKAN=1", "OLLAMA_HOST=127.0.0.1:11434", "OLLAMA_KEEP_ALIVE=5m")
	if runtime.GOOS == "windows" {
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	}
	if err := cmd.Start(); err == nil {
		ollamaCmd = cmd
		weStartedOll = true
	}
	for i := 0; i < 12; i++ {
		time.Sleep(400 * time.Millisecond)
		if isOllamaAlive() {
			return true
		}
	}
	return false
}

func shutdownAll() {
	if weStartedOll && ollamaCmd != nil && ollamaCmd.Process != nil {
		_ = ollamaCmd.Process.Kill()
	}
	os.Exit(0)
}

func runOllamaSegment(model, system, instruction, text string) (string, error) {
	prompt := strings.TrimSpace(instruction)
	if strings.TrimSpace(text) != "" {
		if prompt != "" {
			prompt = fmt.Sprintf("%s:\n\n%s", prompt, strings.TrimSpace(text))
		} else {
			prompt = strings.TrimSpace(text)
		}
	}
	bodyMap := map[string]any{
		"model":  model,
		"prompt": prompt,
		"stream": false,
	}
	if strings.TrimSpace(system) != "" {
		bodyMap["system"] = strings.TrimSpace(system)
	}
	payload, _ := json.Marshal(bodyMap)
	resp, err := http.Post("http://127.0.0.1:11434/api/generate", "application/json", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var ollamaRes struct {
		Response string `json:"response"`
		Error    string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ollamaRes); err != nil {
		return "", err
	}
	if ollamaRes.Error != "" {
		return "", fmt.Errorf("%s", ollamaRes.Error)
	}
	return strings.TrimSpace(ollamaRes.Response), nil
}

func openBrowser(url string) {
	if runtime.GOOS == "windows" {
		cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		_ = cmd.Start()
	}
}

func windowsNativeDialog(psScript string) (string, error) {
	wrapped := fmt.Sprintf(`
Add-Type -AssemblyName System.Windows.Forms
$owner = New-Object System.Windows.Forms.Form
$owner.TopMost = $true
$owner.StartPosition = 'Manual'
$owner.Location = New-Object System.Drawing.Point(-32000, -32000)
$owner.Size = New-Object System.Drawing.Size(1, 1)
$owner.Show()
$owner.Activate()
%s
$owner.Close()
$owner.Dispose()
`, psScript)

	cmd := exec.Command("powershell", "-NoProfile", "-STA", "-Command", wrapped)
	if runtime.GOOS == "windows" {
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	}
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func isSupportedDocExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".txt", ".md", ".json", ".csv", ".pdf", ".docx", ".log", ".yaml", ".yml":
		return true
	}
	return false
}

// syncWithDocumentsFolders keeps workbench_state.json in sync with documents/ and documents/junk/
func syncWithDocumentsFolders(st *WorkbenchState) {
	activeFiles := map[string]string{}
	junkFiles := map[string]string{}

	if entries, err := os.ReadDir(docsDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			ext := filepath.Ext(e.Name())
			if !isSupportedDocExt(ext) {
				continue
			}
			title := strings.TrimSuffix(e.Name(), ext)
			activeFiles[strings.ToLower(title)] = filepath.Join(docsDir, e.Name())
		}
	}

	if entries, err := os.ReadDir(junkDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			ext := filepath.Ext(e.Name())
			if !isSupportedDocExt(ext) {
				continue
			}
			title := strings.TrimSuffix(e.Name(), ext)
			junkFiles[strings.ToLower(title)] = filepath.Join(junkDir, e.Name())
		}
	}

	seenTitles := map[string]bool{}
	var keptNotes []Note

	usedIDs := map[string]bool{}
	for idx, n := range st.Notes {
		if strings.TrimSpace(n.ID) == "" || usedIDs[n.ID] {
			n.ID = fmt.Sprintf("note_%d_%d", time.Now().UnixNano(), idx)
		}
		usedIDs[n.ID] = true
		key := strings.ToLower(sanitizeFilename(n.Title))
		seenTitles[key] = true

		if _, inJunk := junkFiles[key]; inJunk {
			n.Trashed = true
			keptNotes = append(keptNotes, n)
			continue
		}
		if aPath, inActive := activeFiles[key]; inActive {
			n.Trashed = false
			if raw, err := os.ReadFile(aPath); err == nil && strings.TrimSpace(string(raw)) != "" {
				n.Content = string(raw)
			}
			keptNotes = append(keptNotes, n)
			continue
		}
		// Materialize existing state notes into documents/ (or documents/junk/) so the folder is never empty
		if strings.TrimSpace(n.Content) != "" || strings.TrimSpace(n.Title) != "" {
			destDir := docsDir
			if n.Trashed {
				destDir = junkDir
			}
			dest := filepath.Join(destDir, sanitizeFilename(n.Title)+".txt")
			_ = os.WriteFile(dest, []byte(n.Content), 0644)
			keptNotes = append(keptNotes, n)
		}
	}

	// Ingest any new files placed directly into documents/
	for key, fullPath := range activeFiles {
		if seenTitles[key] {
			continue
		}
		baseName := filepath.Base(fullPath)
		ext := filepath.Ext(baseName)
		title := strings.TrimSuffix(baseName, ext)
		var txt string
		var err error
		if strings.ToLower(ext) == ".txt" || strings.ToLower(ext) == ".md" {
			var b []byte
			b, err = os.ReadFile(fullPath)
			txt = string(b)
		} else {
			txt, err = ExtractTextFromPath(fullPath)
		}
		if err != nil {
			continue
		}
		srcLabel := ""
		if strings.ToLower(ext) != ".txt" {
			srcLabel = baseName
		}
		keptNotes = append(keptNotes, Note{
			ID:         fmt.Sprintf("doc_%d_%s", time.Now().UnixNano(), sanitizeFilename(title)),
			Title:      title,
			Content:    txt,
			Color:      "default",
			Tags:       []string{},
			Trashed:    false,
			SourceName: srcLabel,
			UpdatedAt:  time.Now().UnixMilli(),
		})
	}

	// Ingest any files sitting in documents/junk/
	for key, fullPath := range junkFiles {
		if seenTitles[key] {
			continue
		}
		baseName := filepath.Base(fullPath)
		ext := filepath.Ext(baseName)
		title := strings.TrimSuffix(baseName, ext)
		txt, _ := ExtractTextFromPath(fullPath)
		keptNotes = append(keptNotes, Note{
			ID:         fmt.Sprintf("junk_%d_%s", time.Now().UnixNano(), sanitizeFilename(title)),
			Title:      title,
			Content:    txt,
			Color:      "default",
			Tags:       []string{},
			Trashed:    true,
			SourceName: baseName,
			UpdatedAt:  time.Now().UnixMilli(),
		})
	}

	if keptNotes == nil {
		keptNotes = []Note{}
	}
	st.Notes = keptNotes
}

func main() {
	exePath, err := os.Executable()
	if err == nil {
		baseDir = filepath.Dir(exePath)
	} else {
		baseDir, _ = os.Getwd()
	}

	docsDir = filepath.Join(baseDir, "documents")
	junkDir = filepath.Join(docsDir, "junk")
	_ = os.MkdirAll(junkDir, 0755)
	stateFile = filepath.Join(baseDir, "workbench_state.json")

	// Migrate any legacy notes/ files into documents/
	legacyNotes := filepath.Join(baseDir, "notes")
	if entries, err := os.ReadDir(legacyNotes); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				src := filepath.Join(legacyNotes, e.Name())
				dst := filepath.Join(docsDir, e.Name())
				if _, err := os.Stat(dst); os.IsNotExist(err) {
					_ = os.Rename(src, dst)
				}
			}
		}
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&activeClients, 1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})

	http.HandleFunc("/api/ping", func(w http.ResponseWriter, r *http.Request) {
		atomic.StoreInt32(&activeClients, 1)
		w.WriteHeader(http.StatusOK)
	})

	http.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		stateMu.Lock()
		defer stateMu.Unlock()

		if r.Method == http.MethodGet {
			var st WorkbenchState
			if data, err := os.ReadFile(stateFile); err == nil {
				_ = json.Unmarshal(data, &st)
			}
			syncWithDocumentsFolders(&st)
			out, _ := json.MarshalIndent(st, "", "  ")
			_ = os.WriteFile(stateFile, out, 0644)
			w.Header().Set("Content-Type", "application/json")
			w.Write(out)
			return
		}

		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "Read failed", http.StatusBadRequest)
				return
			}
			var st WorkbenchState
			if err := json.Unmarshal(body, &st); err != nil {
				http.Error(w, "Invalid JSON", http.StatusBadRequest)
				return
			}

			// Always store editable working copies in documents/ (never overwrite original .docx/.pdf files)
			for i := range st.Notes {
				n := &st.Notes[i]
				safeName := sanitizeFilename(n.Title) + ".txt"
				activePath := filepath.Join(docsDir, safeName)
				junkPath := filepath.Join(junkDir, safeName)

				if n.Trashed {
					if _, err := os.Stat(activePath); err == nil {
						_ = os.Rename(activePath, junkPath)
					} else if _, err := os.Stat(junkPath); os.IsNotExist(err) {
						_ = os.WriteFile(junkPath, []byte(n.Content), 0644)
					}
				} else {
					if _, err := os.Stat(junkPath); err == nil {
						_ = os.Rename(junkPath, activePath)
					}
					if strings.TrimSpace(n.Content) != "" {
						_ = os.WriteFile(activePath, []byte(n.Content), 0644)
					} else if info, err := os.Stat(activePath); os.IsNotExist(err) || info.Size() == 0 {
						_ = os.WriteFile(activePath, []byte(n.Content), 0644)
					}
				}
			}

			pretty, _ := json.MarshalIndent(st, "", "  ")
			_ = os.WriteFile(stateFile, pretty, 0644)
			w.Header().Set("Content-Type", "application/json")
			w.Write(pretty)
			return
		}
	})

	http.HandleFunc("/api/delete-permanent", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Title string `json:"title"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
			safeName := sanitizeFilename(req.Title) + ".txt"
			_ = os.Remove(filepath.Join(junkDir, safeName))
			_ = os.Remove(filepath.Join(docsDir, safeName))
		}
		w.WriteHeader(http.StatusOK)
	})

	http.HandleFunc("/api/save-as", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Title   string `json:"title"`
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		safeTitle := sanitizeFilename(req.Title)
		escapedInitDir := strings.ReplaceAll(docsDir, "'", "''")
		escapedFileName := strings.ReplaceAll(safeTitle+".txt", "'", "''")

		ps := fmt.Sprintf(`
$dlg = New-Object System.Windows.Forms.SaveFileDialog
$dlg.InitialDirectory = '%s'
$dlg.FileName = '%s'
$dlg.Filter = 'Text File (*.txt)|*.txt|Markdown (*.md)|*.md|JSON (*.json)|*.json|All Files (*.*)|*.*'
$dlg.AddExtension = $true
if ($dlg.ShowDialog($owner) -eq [System.Windows.Forms.DialogResult]::OK) {
    Write-Output $dlg.FileName
}
`, escapedInitDir, escapedFileName)

		chosen, err := windowsNativeDialog(ps)
		if err != nil {
			http.Error(w, "Dialog failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if chosen == "" {
			json.NewEncoder(w).Encode(map[string]any{"canceled": true})
			return
		}
		if err := os.WriteFile(chosen, []byte(req.Content), 0644); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"canceled": false, "path": chosen})
	})

	// Read-only source file ingestion: extracts text & creates a working copy in documents/
	http.HandleFunc("/api/link-file", func(w http.ResponseWriter, r *http.Request) {
		escapedInitDir := strings.ReplaceAll(docsDir, "'", "''")
		ps := fmt.Sprintf(`
$dlg = New-Object System.Windows.Forms.OpenFileDialog
$dlg.InitialDirectory = '%s'
$dlg.Filter = 'Documents (*.txt;*.md;*.json;*.pdf;*.docx)|*.txt;*.md;*.json;*.pdf;*.docx|All Files (*.*)|*.*'
if ($dlg.ShowDialog($owner) -eq [System.Windows.Forms.DialogResult]::OK) {
    Write-Output $dlg.FileName
}
`, escapedInitDir)

		chosen, err := windowsNativeDialog(ps)
		if err != nil {
			http.Error(w, "Dialog failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if chosen == "" {
			json.NewEncoder(w).Encode(map[string]any{"canceled": true})
			return
		}
		txt, err := ExtractTextFromPath(chosen)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		baseName := filepath.Base(chosen)
		ext := filepath.Ext(baseName)
		title := strings.TrimSuffix(baseName, ext)

		// Save working copy into documents/<title>.txt without touching the original file
		docCopyPath := filepath.Join(docsDir, sanitizeFilename(title)+".txt")
		_ = os.WriteFile(docCopyPath, []byte(txt), 0644)

		json.NewEncoder(w).Encode(map[string]any{
			"canceled":   false,
			"sourceName": baseName,
			"title":      title,
			"content":    txt,
		})
	})

	http.HandleFunc("/api/import", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, "Parse error", http.StatusBadRequest)
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "Missing file", http.StatusBadRequest)
			return
		}
		defer file.Close()

		tmpPath := filepath.Join(os.TempDir(), sanitizeFilename(header.Filename))
		out, err := os.Create(tmpPath)
		if err != nil {
			http.Error(w, "Temp error", http.StatusInternalServerError)
			return
		}
		_, _ = io.Copy(out, file)
		out.Close()
		defer os.Remove(tmpPath)

		txt, err := ExtractTextFromPath(tmpPath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ext := filepath.Ext(header.Filename)
		title := strings.TrimSuffix(header.Filename, ext)

		// Write working copy straight into documents/<title>.txt
		docCopyPath := filepath.Join(docsDir, sanitizeFilename(title)+".txt")
		_ = os.WriteFile(docCopyPath, []byte(txt), 0644)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"title":      title,
			"sourceName": header.Filename,
			"content":    txt,
		})
	})

	http.HandleFunc("/api/ollama", func(w http.ResponseWriter, r *http.Request) {
		if !ensureOllamaRunning() {
			http.Error(w, "Could not start Ollama", http.StatusBadGateway)
			return
		}
		if r.Method == http.MethodGet {
			resp, err := http.Get("http://127.0.0.1:11434/api/tags")
			if err != nil {
				http.Error(w, "Ollama offline", http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
			w.Header().Set("Content-Type", "application/json")
			io.Copy(w, resp.Body)
			return
		}

		var req struct {
			Model       string `json:"model"`
			System      string `json:"system"`
			Instruction string `json:"instruction"`
			Text        string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		if req.Model == "" {
			req.Model = "huihui_ai/qwen2.5-abliterate:3b"
		}

		const soundTag = "[overall_soundscape]"
		if idx := strings.Index(req.Text, soundTag); idx != -1 {
			visualPart := strings.TrimSpace(req.Text[:idx])
			soundPart := strings.TrimSpace(req.Text[idx+len(soundTag):])

			visualOut, err := runOllamaSegment(req.Model, req.System, req.Instruction, visualPart)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			finalOut := visualOut
			if soundPart != "" {
				soundOut, err := runOllamaSegment(req.Model, req.System, req.Instruction, soundPart)
				if err == nil && soundOut != "" {
					finalOut = visualOut + "\n\n" + soundTag + "\n" + soundOut
				} else {
					finalOut = visualOut + "\n\n" + soundTag + "\n" + soundPart
				}
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"response": finalOut})
			return
		}

		out, err := runOllamaSegment(req.Model, req.System, req.Instruction, req.Text)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"response": out})
	})

	// Clean shutdown when browser tab closes (with 1.5s grace window so F5 refresh doesn't kill the server)
	http.HandleFunc("/api/tab-closed", func(w http.ResponseWriter, r *http.Request) {
		atomic.StoreInt32(&activeClients, 0)
		w.WriteHeader(http.StatusOK)
		go func() {
			time.Sleep(1500 * time.Millisecond)
			if atomic.LoadInt32(&activeClients) == 0 {
				shutdownAll()
			}
		}()
	})

	http.HandleFunc("/api/shutdown", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		go func() {
			time.Sleep(150 * time.Millisecond)
			shutdownAll()
		}()
	})

	port := ":8080"
	go func() {
		time.Sleep(350 * time.Millisecond)
		openBrowser("http://localhost:8080/")
	}()

	if err := http.ListenAndServe(port, nil); err != nil {
		openBrowser("http://localhost:8080/")
	}
}
