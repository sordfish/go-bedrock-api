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

// ManifestHeader represents the header section of a manifest.json.
type ManifestHeader struct {
	UUID    string `json:"uuid"`
	Version []int  `json:"version"`
}

// Manifest represents the structure of a manifest.json file.
type Manifest struct {
	Header ManifestHeader `json:"header"`
}

// writeJSONError sends an error response in JSON format.
func writeJSONError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	resp := map[string]string{"error": message}
	json.NewEncoder(w).Encode(resp)
}

// writeJSONResponse sends a successful response in JSON format.
func writeJSONResponse(w http.ResponseWriter, code int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(payload)
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
				return filepath.Join("/data/worlds", levelName), nil
			}
		}
	}
	return "", fmt.Errorf("level-name not found in %s", serverPropsPath)
}

// sendCommandHandler reads a command from the POST body and writes it to the FIFO.
func sendCommandHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Printf("Error reading request body: %v", err)
		writeJSONError(w, http.StatusBadRequest, "Bad Request")
		return
	}
	defer r.Body.Close()
	command := strings.TrimSpace(string(body))
	if command == "" {
		writeJSONError(w, http.StatusBadRequest, "Empty command")
		return
	}
	fifo, err := os.OpenFile(fifoPath, os.O_WRONLY, 0)
	if err != nil {
		log.Printf("Error opening FIFO file: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	defer fifo.Close()
	_, err = fifo.Write([]byte(command + "\n"))
	if err != nil {
		log.Printf("Error writing to FIFO: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	log.Printf("Command sent: %s", command)
	writeJSONResponse(w, http.StatusOK, map[string]string{"message": "Command sent successfully"})
}

// listAddonsHandler lists directories in the behavior and resource packs directories.
func listAddonsHandler(w http.ResponseWriter, r *http.Request) {
	behaviorAddons, err := listDirectories(behaviorPacksDir)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Failed to list behavior packs")
		return
	}
	resourceAddons, err := listDirectories(resourcePacksDir)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Failed to list resource packs")
		return
	}
	result := map[string][]string{
		"behavior_packs": behaviorAddons,
		"resource_packs": resourceAddons,
	}
	writeJSONResponse(w, http.StatusOK, result)
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
		writeJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		writeJSONError(w, http.StatusBadRequest, "File too big")
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		log.Printf("Error retrieving file from form: %v", err)
		writeJSONError(w, http.StatusBadRequest, "Bad Request")
		return
	}
	defer file.Close()
	tmpFile, err := ioutil.TempFile("", "upload-*.mcaddon")
	if err != nil {
		log.Printf("Error creating temp file: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	defer os.Remove(tmpFile.Name())
	data, err := ioutil.ReadAll(file)
	if err != nil {
		log.Printf("Error reading uploaded file: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	if _, err = tmpFile.Write(data); err != nil {
		log.Printf("Error writing to temp file: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	tmpFile.Close()
	zipReader, err := zip.OpenReader(tmpFile.Name())
	if err != nil {
		log.Printf("Error opening zip archive: %v", err)
		writeJSONError(w, http.StatusBadRequest, "Invalid mcaddon file")
		return
	}
	defer zipReader.Close()
	extractDir, err := ioutil.TempDir("", "mcaddon-extract")
	if err != nil {
		log.Printf("Error creating temporary extraction directory: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	defer os.RemoveAll(extractDir)
	for _, f := range zipReader.File {
		fpath := filepath.Join(extractDir, f.Name)
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
			writeJSONError(w, http.StatusInternalServerError, "Failed to install behavior pack")
			return
		}
	}
	if dirExists(resourceSrc) {
		err = copyDir(resourceSrc, resourcePacksDir)
		if err != nil {
			log.Printf("Error copying resource pack: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "Failed to install resource pack")
			return
		}
	}
	writeJSONResponse(w, http.StatusOK, map[string]string{"message": "mcaddon processed and installed successfully"})
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

// getInstalledAddons scans all directories in packDir, reads the manifest.json (if available),
// and returns a map of manifest UUIDs to their directory paths.
func getInstalledAddons(packDir string) (map[string]string, error) {
	installed := make(map[string]string)
	dirs, err := ioutil.ReadDir(packDir)
	if err != nil {
		return installed, err
	}
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		manifestPath := filepath.Join(packDir, dir.Name(), "manifest.json")
		data, err := ioutil.ReadFile(manifestPath)
		if err != nil {
			log.Printf("Could not read manifest.json in %s: %v", dir.Name(), err)
			continue
		}
		var manifest Manifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			log.Printf("Error parsing manifest.json in %s: %v", dir.Name(), err)
			continue
		}
		installed[manifest.Header.UUID] = filepath.Join(packDir, dir.Name())
	}
	return installed, nil
}

// getActiveAddons reads the world JSON file containing an array of ActiveAddon,
// then checks each addon against installed addons (by scanning manifest.json files in packDir).
func getActiveAddons(jsonPath, packDir string) ([]ActiveAddon, error) {
	data, err := ioutil.ReadFile(jsonPath)
	if err != nil {
		return nil, err
	}
	var addons []ActiveAddon
	if err := json.Unmarshal(data, &addons); err != nil {
		return nil, err
	}
	installed, err := getInstalledAddons(packDir)
	if err != nil {
		return nil, err
	}
	validAddons := []ActiveAddon{}
	for _, addon := range addons {
		if _, found := installed[addon.PackID]; found {
			validAddons = append(validAddons, addon)
		} else {
			log.Printf("Installed addon not found for pack_id: %s", addon.PackID)
		}
	}
	return validAddons, nil
}

// activeAddonsHandler reads the active addons JSON files from the world folder,
// then matches installed addons by scanning each pack's manifest.json in the corresponding packs directories.
// It supports both "behavior" and "behaviour" spellings for the behavior packs JSON file.
// If the required JSON files are missing, it returns a 404.
func activeAddonsHandler(w http.ResponseWriter, r *http.Request) {
	worldFolder, err := getWorldFolder()
	if err != nil {
		log.Printf("Error getting world folder: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "Error determining world folder")
		return
	}
	// Check for both American and British spellings.
	behaviorJSON1 := filepath.Join(worldFolder, "world_behavior_packs.json")
	behaviorJSON2 := filepath.Join(worldFolder, "world_behaviour_packs.json")
	var behaviorJSON string
	if _, err := os.Stat(behaviorJSON1); err == nil {
		behaviorJSON = behaviorJSON1
	} else if _, err := os.Stat(behaviorJSON2); err == nil {
		behaviorJSON = behaviorJSON2
	} else {
		writeJSONError(w, http.StatusNotFound, "world_behavior_packs.json not found")
		return
	}
	resourceJSON := filepath.Join(worldFolder, "world_resource_packs.json")
	if _, err := os.Stat(resourceJSON); os.IsNotExist(err) {
		writeJSONError(w, http.StatusNotFound, "world_resource_packs.json not found")
		return
	}
	behaviorAddons, err := getActiveAddons(behaviorJSON, behaviorPacksDir)
	if err != nil {
		log.Printf("Error reading active behavior addons: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "Error reading active behavior addons")
		return
	}
	resourceAddons, err := getActiveAddons(resourceJSON, resourcePacksDir)
	if err != nil {
		log.Printf("Error reading active resource addons: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "Error reading active resource addons")
		return
	}
	result := map[string]interface{}{
		"active_behavior_addons": behaviorAddons,
		"active_resource_addons": resourceAddons,
	}
	writeJSONResponse(w, http.StatusOK, result)
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
