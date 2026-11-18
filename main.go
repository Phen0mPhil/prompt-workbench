package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

//go:embed frontend.html
var frontend embed.FS

type Note struct {
	ID        int64    `json:"id"`
	Title     string   `json:"title"`
	Tags      []string `json:"tags"`
	BodyHTML  string   `json:"bodyHtml"`
	Color     string   `json:"color"`
	FilePath  string   `json:"filePath,omitempty"` // Pointer to external file on disk
	Trashed   bool     `json:"trashed"`
	UpdatedAt int64    `json:"updatedAt"`
}

type AppState struct {
	Notes        []Note  `json:"notes"`
	WorkspaceIDs []int64 `json:"workspaceIds"`
}

var (
	stateMu sync.Mutex
	dbFile  = "workbench_state.json"
)

func getAppDir() string {
	exe, err := os.Executable()
	if err != nil {
		cwd, _ := os.Getwd()
		return cwd
	}
	dir := filepath.Dir(exe)
	if strings.Contains(dir, os.TempDir()) || strings.Contains(dir, "go-build") {
		cwd, _ := os.Getwd()
		return cwd
	}
	return dir
}

func getDBPath() string {
	return filepath.Join(getAppDir(), dbFile)
}

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		html, err := frontend.ReadFile("frontend.html")
		if err != nil {
			http.Error(w, "Missing frontend.html", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(html)
	})

	// Load & Save full session state (notes, pointers, workspace panes, trash)
	http.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		stateMu.Lock()
		defer stateMu.Unlock()
		path := getDBPath()

		if r.Method == http.MethodGet {
			data, err := os.ReadFile(path)
			if err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"notes":[],"workspaceIds":[]}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
			return
		}

		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "Failed to read state", http.StatusBadRequest)
				return
			}
			if err := os.WriteFile(path, body, 0644); err != nil {
				http.Error(w, "Failed to write state", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		}
	})

	// Link a local file by path pointer (no heavy duplication) or upload fallback
	http.HandleFunc("/api/link-file", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			FilePath string `json:"filePath"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.FilePath) == "" {
			http.Error(w, "Invalid file path", http.StatusBadRequest)
			return
		}
		cleanPath := strings.Trim(req.FilePath, `"' `)
		text, err := ExtractTextFromPath(cleanPath)
		if err != nil {
			http.Error(w, "Could not read file: "+err.Error(), http.StatusBadRequest)
			return
		}

		base := filepath.Base(cleanPath)
		title := strings.TrimSuffix(base, filepath.Ext(base))

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"title":    title,
			"content":  text,
			"filePath": cleanPath,
		})
	})

	// Multipart file import fallback (when picking via browser dialog)
	http.HandleFunc("/api/import", func(w http.ResponseWriter, r *http.Request) {
		r.ParseMultipartForm(50 << 20)
		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "Failed to read upload", http.StatusBadRequest)
			return
		}
		defer file.Close()

		data, _ := io.ReadAll(file)
		text, err := ExtractText(header.Filename, data)
		if err != nil {
			http.Error(w, "Parse error: "+err.Error(), http.StatusInternalServerError)
			return
		}

		title := strings.TrimSuffix(header.Filename, filepath.Ext(header.Filename))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"title":   title,
			"content": text,
		})
	})

	// Export note directly to a chosen local path
	http.HandleFunc("/api/export", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			FilePath string `json:"filePath"`
			Content  string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.FilePath) == "" {
			http.Error(w, "Invalid export request", http.StatusBadRequest)
			return
		}
		cleanPath := strings.Trim(req.FilePath, `"' `)
		if err := os.MkdirAll(filepath.Dir(cleanPath), 0755); err != nil {
			http.Error(w, "Directory error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := os.WriteFile(cleanPath, []byte(req.Content), 0644); err != nil {
			http.Error(w, "Save failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	// Save literal note to disk and return canonical file path
	// Native OS Folder Picker (Windows FolderBrowserDialog / Linux zenity)
	http.HandleFunc("/api/pick-folder", func(w http.ResponseWriter, r *http.Request) {
		var out []byte
		var err error
		if runtime.GOOS == "windows" {
			ps := `Add-Type -AssemblyName System.Windows.Forms; $f = New-Object System.Windows.Forms.FolderBrowserDialog; $f.Description = 'Select Save Directory'; if($f.ShowDialog() -eq 'OK'){ Write-Output $f.SelectedPath }`
			out, err = exec.Command("powershell", "-NoProfile", "-Command", ps).Output()
		} else {
			out, err = exec.Command("zenity", "--file-selection", "--directory", "--title=Select Save Directory").Output()
		}
		if err != nil {
			http.Error(w, "Folder picker cancelled or unavailable", http.StatusBadRequest)
			return
		}
		folder := strings.TrimSpace(string(out))
		if folder == "" {
			http.Error(w, "No folder selected", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"folder": folder})
	})

	// Write literal note file into ./notes/ next to the executable
	http.HandleFunc("/api/save-default", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			FileName string `json:"fileName"`
			Content  string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.FileName) == "" {
			http.Error(w, "Invalid request", http.StatusBadRequest)
			return
		}
		notesDir := filepath.Join(getAppDir(), "notes")
		if err := os.MkdirAll(notesDir, 0755); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fullPath := filepath.Join(notesDir, filepath.Base(req.FileName))
		if err := os.WriteFile(fullPath, []byte(req.Content), 0644); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"filePath": fullPath})
	})

	http.HandleFunc("/api/save-note", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			FilePath string `json:"filePath"`
			Content  string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.FilePath) == "" {
			http.Error(w, "Invalid save path", http.StatusBadRequest)
			return
		}
		cleanPath := strings.Trim(req.FilePath, `"' `)
		if err := os.MkdirAll(filepath.Dir(cleanPath), 0755); err != nil {
			http.Error(w, "Directory error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := os.WriteFile(cleanPath, []byte(req.Content), 0644); err != nil {
			http.Error(w, "Save failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"filePath": cleanPath,
		})
	})

	port := ":8080"
	fmt.Println("Workbench active at http://localhost" + port)
	openBrowser("http://localhost" + port)
	log.Fatal(http.ListenAndServe(port, nil))
}

func openBrowser(url string) {
	var err error
	switch runtime.GOOS {
	case "windows":
		err = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "linux":
		err = exec.Command("xdg-open", url).Start()
	}
	if err != nil {
		log.Println("Open browser manually:", url)
	}
}
