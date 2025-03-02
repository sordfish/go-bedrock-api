package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	fifoPath               = "/shared/command_fifo"
	behaviorPacksDir       = "/data/behavior_packs"
	resourcePacksDir       = "/data/resource_packs"
	serverPropsPath        = "/data/server.properties"
	maxUploadSize    int64 = 10 << 20 // 10 MB
)

// ActiveAddon represents an entry in the world JSON files.
type ActiveAddon struct {
	PackID  string `json:"pack_id"`
	Version []int  `json:"version"`
}

// getWorldFolder reads /data/server.properties, extracts the level-name value,
// and returns the world folder path as "/data/worlds/<level-name>".
func getWorldFolder() (string, error) {
	data, err := ioutil.ReadFile(serverPropsPath)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		// Skip comments and empty lines.
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "level-name=") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				levelName := strings.TrimSpace(parts[1])
				if levelName == "" {
					return "", fmt.Errorf("level-name is empty in server.properties")
				}
				// Construct the world folder path.
				return filepath.Join("/data/worlds", levelName), nil
			}
		}
	}
	return "", fmt.Errorf("level-name not found in %s", serverPropsPath)
}

// sendCommandHandler reads the command from the POST body and writes it to the FIFO.
func sendCommandHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Printf("Error reading request body: %v", err)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	command := strings.TrimSpace(string(body))
	if command == "" {
		http.Error(w, "Empty command", http.StatusBadRequest)
		return
	}

	fifo, err := os.OpenFile(fifoPath, os.O_WRONLY, 0)
	if err != nil {
		log.Printf("Error opening FIFO file: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer fifo.Close()

	_, err = fifo.Write([]byte(command + "\n"))
	if err != nil {
		log.Printf("Error writing to FIFO: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	log.Printf("Command sent: %s", command)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Command sent successfully"))
}

// listAddonsHandler lists directories in the behavior and resource packs directories.
func listAddonsHandler(w http.ResponseWriter, r *http.Request) {
	behaviorAddons, err := listDirectories(behaviorPacksDir)
	if err != nil {
		http.Error(w, "Failed to list behavior packs", http.StatusInternalServerError)
		return
	}
	resourceAddons, err := listDirectories(resourcePacksDir)
	if err != nil {
		http.Error(w, "Failed to list resource packs", http.StatusInternalServerError)
		return
	}
	result := map[string][]string{
		"behavior_packs": behaviorAddons,
		"resource_packs": resourceAddons,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func listDirectories(dir string) ([]string, error) {
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, file := range files {
		if file.IsDir() {
			dirs = append(dirs, file.Name())
		}
	}
	return dirs, nil
}

// uploadMcAddonHandler accepts an mcaddon file upload, extracts it,
// and copies the behavior and resource packs to the appropriate folders.
func uploadMcAddonHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		http.Error(w, "File too big", http.StatusBadRequest)
		return
	}

	file, _, err := r.FormFile("file")
	if err != nil {
		log.Printf("Error retrieving file from form: %v", err)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	defer file.Close()

	tmpFile, err := ioutil.TempFile("", "upload-*.mcaddon")
	if err != nil {
		log.Printf("Error creating temp file: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer os.Remove(tmpFile.Name())
	data, err := ioutil.ReadAll(file)
	if err != nil {
		log.Printf("Error reading uploaded file: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if _, err = tmpFile.Write(data); err != nil {
		log.Printf("Error writing to temp file: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	tmpFile.Close()

	zipReader, err := zip.OpenReader(tmpFile.Name())
	if err != nil {
		log.Printf("Error opening zip archive: %v", err)
		http.Error(w, "Invalid mcaddon file", http.StatusBadRequest)
		return
	}
	defer zipReader.Close()

	extractDir, err := ioutil.TempDir("", "mcaddon-extract")
	if err != nil {
		log.Printf("Error creating temporary extraction directory: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(extractDir)

	for _, f := range zipReader.File {
		fpath := filepath.Join(extractDir, f.Name)
		// Security check to avoid ZipSlip vulnerability.
		if !strings.HasPrefix(fpath, filepath.Clean(extractDir)+string(os.PathSeparator)) {
			log.Printf("illegal file path: %s", fpath)
			continue
		}
		if f.FileInfo().IsDir() {
			os.MkdirAll(fpath, os.ModePerm)
			continue
		}
		if err = os.MkdirAll(filepath.Dir(fpath), os.ModePerm); err != nil {
			log.Printf("Error creating directory: %v", err)
			continue
		}
		outFile, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			log.Printf("Error opening file for extraction: %v", err)
			continue
		}
		rc, err := f.Open()
		if err != nil {
			outFile.Close()
			log.Printf("Error opening file in zip: %v", err)
			continue
		}
		_, err = io.Copy(outFile, rc)
		outFile.Close()
		rc.Close()
		if err != nil {
			log.Printf("Error extracting file: %v", err)
			continue
		}
	}

	// Assume the extracted archive contains two folders at its root: "behavior" and "resource".
	behaviorSrc := filepath.Join(extractDir, "behavior")
	resourceSrc := filepath.Join(extractDir, "resource")

	if dirExists(behaviorSrc) {
		err = copyDir(behaviorSrc, behaviorPacksDir)
		if err != nil {
			log.Printf("Error copying behavior pack: %v", err)
			http.Error(w, "Failed to install behavior pack", http.StatusInternalServerError)
			return
		}
	}
	if dirExists(resourceSrc) {
		err = copyDir(resourceSrc, resourcePacksDir)
		if err != nil {
			log.Printf("Error copying resource pack: %v", err)
			http.Error(w, "Failed to install resource pack", http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("mcaddon processed and installed successfully"))
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// copyDir recursively copies a directory tree from src to dst.
func copyDir(src string, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relPath, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		dstPath := filepath.Join(dst, relPath)
		if info.IsDir() {
			return os.MkdirAll(dstPath, info.Mode())
		}
		srcFile, err := os.Open(path)
		if err != nil {
			return err
		}
		defer srcFile.Close()
		dstFile, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY, info.Mode())
		if err != nil {
			return err
		}
		defer dstFile.Close()
		_, err = io.Copy(dstFile, srcFile)
		return err
	})
}

// getActiveAddons reads a JSON file containing an array of ActiveAddon and verifies existence in packDir.
func getActiveAddons(jsonPath, packDir string) ([]ActiveAddon, error) {
	data, err := ioutil.ReadFile(jsonPath)
	if err != nil {
		return nil, err
	}
	var addons []ActiveAddon
	if err := json.Unmarshal(data, &addons); err != nil {
		return nil, err
	}
	validAddons := []ActiveAddon{}
	for _, addon := range addons {
		addonPath := filepath.Join(packDir, addon.PackID)
		if info, err := os.Stat(addonPath); err == nil && info.IsDir() {
			validAddons = append(validAddons, addon)
		} else {
			log.Printf("Addon folder for pack_id %s not found in %s", addon.PackID, packDir)
		}
	}
	return validAddons, nil
}

// activeAddonsHandler reads the active addons JSON files from the world folder,
// then checks for matching addon directories in the corresponding packs directories.
func activeAddonsHandler(w http.ResponseWriter, r *http.Request) {
	worldFolder, err := getWorldFolder()
	if err != nil {
		log.Printf("Error getting world folder: %v", err)
		http.Error(w, "Error determining world folder", http.StatusInternalServerError)
		return
	}
	behaviorJSON := filepath.Join(worldFolder, "world_behavior_packs.json")
	resourceJSON := filepath.Join(worldFolder, "world_resource_packs.json")

	behaviorAddons, err := getActiveAddons(behaviorJSON, behaviorPacksDir)
	if err != nil {
		log.Printf("Error reading active behavior addons: %v", err)
		http.Error(w, "Error reading active behavior addons", http.StatusInternalServerError)
		return
	}
	resourceAddons, err := getActiveAddons(resourceJSON, resourcePacksDir)
	if err != nil {
		log.Printf("Error reading active resource addons: %v", err)
		http.Error(w, "Error reading active resource addons", http.StatusInternalServerError)
		return
	}

	result := map[string]interface{}{
		"active_behavior_addons": behaviorAddons,
		"active_resource_addons": resourceAddons,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func main() {
	http.HandleFunc("/send-command", sendCommandHandler)
	http.HandleFunc("/list-addons", listAddonsHandler)
	http.HandleFunc("/upload-mcaddon", uploadMcAddonHandler)
	http.HandleFunc("/active-addons", activeAddonsHandler)

	port := "8080"
	log.Printf("Starting sidecar command server on port %s...", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
