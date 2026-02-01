package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	fifoPath                     = "/shared/command_fifo"
	behaviorPacksDir             = "/data/behavior_packs"
	resourcePacksDir             = "/data/resource_packs"
	serverPropsPath              = "/data/server.properties"
	behaviorPackArchiveDir       = "/data/pack_archives/behavior"
	resourcePackArchiveDir       = "/data/pack_archives/resource"
	maxUploadSize          int64 = 10 << 20 // 10 MB
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

// CustomCommand represents a custom command stored in memory
type CustomCommand struct {
	Name        string    `json:"name"`
	Command     string    `json:"command"`
	CreatedAt   time.Time `json:"created_at"`
	ExecutedAt  time.Time `json:"executed_at,omitempty"`
}

// PlayerCoords represents a player's current coordinates
type PlayerCoords struct {
	Name string `json:"name"`
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
	Z    float64 `json:"z"`
}

// Global state for custom commands
var (
	customCommands = make([]CustomCommand, 0)
	commandsMutex  sync.RWMutex
)

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
	data, err := os.ReadFile(serverPropsPath)
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

// ensureArchiveDirectories creates the archive directory structure
func ensureArchiveDirectories() error {
	dirs := []string{behaviorPackArchiveDir, resourcePackArchiveDir}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create archive directory %s: %w", dir, err)
		}
	}
	return nil
}

// getManifestUUID extracts the UUID from a manifest.json file
func getManifestUUID(manifestPath string) (string, error) {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", err
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return "", err
	}
	return manifest.Header.UUID, nil
}

// findPackByUUID searches for a pack directory in a target directory by matching manifest UUID
func findPackByUUID(searchDir, uuid string) (string, error) {
	entries, err := os.ReadDir(searchDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		manifestPath := filepath.Join(searchDir, entry.Name(), "manifest.json")
		foundUUID, err := getManifestUUID(manifestPath)
		if err != nil {
			continue
		}
		if foundUUID == uuid {
			return filepath.Join(searchDir, entry.Name()), nil
		}
	}
	return "", nil
}

// extractMcpackToDir extracts a single mcpack file to a target directory
func extractMcpackToDir(mcpackPath, targetDir string) error {
	reader, err := zip.OpenReader(mcpackPath)
	if err != nil {
		return fmt.Errorf("failed to open mcpack: %w", err)
	}
	defer reader.Close()

	for _, f := range reader.File {
		fpath := filepath.Join(targetDir, f.Name)
		if !strings.HasPrefix(fpath, filepath.Clean(targetDir)+string(os.PathSeparator)) {
			continue
		}
		if f.FileInfo().IsDir() {
			os.MkdirAll(fpath, os.ModePerm)
			continue
		}
		if err = os.MkdirAll(filepath.Dir(fpath), os.ModePerm); err != nil {
			continue
		}
		outFile, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			outFile.Close()
			continue
		}
		_, err = io.Copy(outFile, rc)
		outFile.Close()
		rc.Close()
		if err != nil {
			continue
		}
	}

	return nil
}

// saveMcpackToArchive saves an mcpack file to the archive directory
func saveMcpackToArchive(mcpackPath, packType string) (string, string, error) {
	var archiveDir string
	if packType == "behavior" {
		archiveDir = behaviorPackArchiveDir
	} else {
		archiveDir = resourcePackArchiveDir
	}

	// Get UUID from the mcpack to create a meaningful filename
	uuid, err := extractPackUUIDFromMcpack(mcpackPath)
	if err != nil {
		uuid = filepath.Base(mcpackPath)
	}

	// Create a subdirectory for this pack
	packDir := filepath.Join(archiveDir, strings.TrimSuffix(uuid, filepath.Ext(uuid)))
	if err := os.MkdirAll(packDir, 0755); err != nil {
		return "", "", fmt.Errorf("failed to create pack archive directory: %w", err)
	}

	archivePath := filepath.Join(packDir, filepath.Base(mcpackPath))
	src, err := os.Open(mcpackPath)
	if err != nil {
		return "", "", fmt.Errorf("failed to open source mcpack: %w", err)
	}
	defer src.Close()

	dst, err := os.Create(archivePath)
	if err != nil {
		return "", "", fmt.Errorf("failed to create archive file: %w", err)
	}
	defer dst.Close()

	if _, err = io.Copy(dst, src); err != nil {
		return "", "", fmt.Errorf("failed to copy mcpack to archive: %w", err)
	}

	return archivePath, packDir, nil
}

// extractPackUUIDFromMcpack reads UUID from manifest.json inside an mcpack
func extractPackUUIDFromMcpack(mcpackPath string) (string, error) {
	reader, err := zip.OpenReader(mcpackPath)
	if err != nil {
		return "", err
	}
	defer reader.Close()

	for _, f := range reader.File {
		if f.Name == "manifest.json" {
			rc, err := f.Open()
			if err != nil {
				continue
			}
			defer rc.Close()

			data, err := io.ReadAll(rc)
			if err != nil {
				continue
			}

			var manifest Manifest
			if err := json.Unmarshal(data, &manifest); err != nil {
				continue
			}
			return manifest.Header.UUID, nil
		}
	}

	return "", fmt.Errorf("manifest.json not found in mcpack")
}

// restoreDeletedPacks checks if installed packs still exist, and if not, extracts them from archives
func restoreDeletedPacks() error {
	log.Println("Checking for deleted packs at startup...")

	// Check behavior packs
	behaviorEntries, err := os.ReadDir(behaviorPackArchiveDir)
	if err == nil {
		for _, entry := range behaviorEntries {
			if !entry.IsDir() {
				continue
			}
			packDir := filepath.Join(behaviorPackArchiveDir, entry.Name())
			if err := restorePackFromArchive(packDir, behaviorPacksDir); err != nil {
				log.Printf("Warning: Failed to restore behavior pack %s: %v", entry.Name(), err)
			}
		}
	}

	// Check resource packs
	resourceEntries, err := os.ReadDir(resourcePackArchiveDir)
	if err == nil {
		for _, entry := range resourceEntries {
			if !entry.IsDir() {
				continue
			}
			packDir := filepath.Join(resourcePackArchiveDir, entry.Name())
			if err := restorePackFromArchive(packDir, resourcePacksDir); err != nil {
				log.Printf("Warning: Failed to restore resource pack %s: %v", entry.Name(), err)
			}
		}
	}

	return nil
}

// restorePackFromArchive extracts a pack if it's missing from the destination directory
func restorePackFromArchive(archivePackDir, destinationDir string) error {
	// Find the mcpack file in the archive directory
	entries, err := os.ReadDir(archivePackDir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		filename := entry.Name()
		if !strings.HasSuffix(strings.ToLower(filename), ".mcpack") && !strings.HasSuffix(strings.ToLower(filename), ".zip") {
			continue
		}

		mcpackPath := filepath.Join(archivePackDir, filename)

		// Extract UUID from mcpack
		uuid, err := extractPackUUIDFromMcpack(mcpackPath)
		if err != nil {
			log.Printf("Could not extract UUID from %s: %v", filename, err)
			continue
		}

		// Check if pack already exists in destination
		existingPath, err := findPackByUUID(destinationDir, uuid)
		if err == nil && existingPath != "" {
			log.Printf("Pack %s already exists at %s", uuid, existingPath)
			continue
		}

		// Pack is missing, extract it
		log.Printf("Restoring pack %s from archive: %s", uuid, mcpackPath)

		tmpDir, err := os.MkdirTemp("", "restore-pack")
		if err != nil {
			return fmt.Errorf("failed to create temp directory: %w", err)
		}
		defer os.RemoveAll(tmpDir)

		if err := extractMcpackToDir(mcpackPath, tmpDir); err != nil {
			return fmt.Errorf("failed to extract mcpack: %w", err)
		}

		// Copy extracted pack to destination
		if err := copyDir(tmpDir, destinationDir); err != nil {
			return fmt.Errorf("failed to copy pack to destination: %w", err)
		}

		log.Printf("Successfully restored pack %s", uuid)
		return nil
	}

	return nil
}

// sendCommandHandler reads a command from the POST body and writes it to the FIFO.
func sendCommandHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	body, err := io.ReadAll(r.Body)
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
	files, err := os.ReadDir(dir)
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
// saves mcpack files to archive, and copies the behavior and resource packs to the appropriate folders.
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

	tmpFile, err := os.CreateTemp("", "upload-*.mcaddon")
	if err != nil {
		log.Printf("Error creating temp file: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	defer os.Remove(tmpFile.Name())

	data, err := io.ReadAll(file)
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

	extractDir, err := os.MkdirTemp("", "mcaddon-extract")
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

	// Process extracted mcpacks - look for them recursively
	behaviorMcpacks := []string{}
	resourceMcpacks := []string{}

	filepath.Walk(extractDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		lower := strings.ToLower(path)
		if !strings.HasSuffix(lower, ".mcpack") && !strings.HasSuffix(lower, ".zip") {
			return nil
		}

		// Try to determine pack type by reading manifest
		reader, err := zip.OpenReader(path)
		if err != nil {
			return nil
		}
		defer reader.Close()

		isResource := false
		for _, f := range reader.File {
			if f.Name == "manifest.json" {
				rc, _ := f.Open()
				if rc != nil {
					data, _ := io.ReadAll(rc)
					rc.Close()
					var manifest Manifest
					if err := json.Unmarshal(data, &manifest); err == nil {
						// Try to identify type from directory structure or manifest
						// For now, we'll check if it's in a "resource" subfolder or similar
						if strings.Contains(filepath.ToSlash(path), "resource") {
							isResource = true
						}
					}
				}
				break
			}
		}

		if isResource {
			resourceMcpacks = append(resourceMcpacks, path)
		} else {
			behaviorMcpacks = append(behaviorMcpacks, path)
		}

		return nil
	})

	// Save behavior packs to archive and extract
	for _, mcpackPath := range behaviorMcpacks {
		archivePath, _, err := saveMcpackToArchive(mcpackPath, "behavior")
		if err != nil {
			log.Printf("Error saving behavior pack to archive: %v", err)
			continue
		}
		log.Printf("Saved behavior pack to archive: %s", archivePath)

		// Extract to installation directory
		tmpExtractDir, err := os.MkdirTemp("", "extract-bp")
		if err != nil {
			log.Printf("Error creating temp extraction dir: %v", err)
			continue
		}
		if err := extractMcpackToDir(mcpackPath, tmpExtractDir); err != nil {
			log.Printf("Error extracting behavior pack: %v", err)
			os.RemoveAll(tmpExtractDir)
			continue
		}
		if err := copyDir(tmpExtractDir, behaviorPacksDir); err != nil {
			log.Printf("Error copying behavior pack: %v", err)
		}
		os.RemoveAll(tmpExtractDir)
	}

	// Save resource packs to archive and extract
	for _, mcpackPath := range resourceMcpacks {
		archivePath, _, err := saveMcpackToArchive(mcpackPath, "resource")
		if err != nil {
			log.Printf("Error saving resource pack to archive: %v", err)
			continue
		}
		log.Printf("Saved resource pack to archive: %s", archivePath)

		// Extract to installation directory
		tmpExtractDir, err := os.MkdirTemp("", "extract-rp")
		if err != nil {
			log.Printf("Error creating temp extraction dir: %v", err)
			continue
		}
		if err := extractMcpackToDir(mcpackPath, tmpExtractDir); err != nil {
			log.Printf("Error extracting resource pack: %v", err)
			os.RemoveAll(tmpExtractDir)
			continue
		}
		if err := copyDir(tmpExtractDir, resourcePacksDir); err != nil {
			log.Printf("Error copying resource pack: %v", err)
		}
		os.RemoveAll(tmpExtractDir)
	}

	writeJSONResponse(w, http.StatusOK, map[string]string{"message": "mcaddon processed and installed successfully"})
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
	dirs, err := os.ReadDir(packDir)
	if err != nil {
		return installed, err
	}
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		manifestPath := filepath.Join(packDir, dir.Name(), "manifest.json")
		data, err := os.ReadFile(manifestPath)
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
	data, err := os.ReadFile(jsonPath)
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

// uiHandler serves the web UI
func uiHandler(w http.ResponseWriter, r *http.Request) {
	html := `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Bedrock Server Control Panel</title>
    <link href="https://cdn.jsdelivr.net/npm/bootstrap@5.3.0/dist/css/bootstrap.min.css" rel="stylesheet">
    <style>
        body {
            background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
            min-height: 100vh;
            padding: 20px;
        }
        .container {
            max-width: 1400px;
        }
        .card {
            box-shadow: 0 10px 30px rgba(0,0,0,0.3);
            border: none;
            border-radius: 10px;
            margin-bottom: 20px;
        }
        .card-header {
            background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
            color: white;
            border-radius: 10px 10px 0 0;
            font-weight: bold;
        }
        .btn {
            border-radius: 5px;
            font-weight: 500;
            margin: 5px;
        }
        .btn-primary {
            background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
            border: none;
        }
        .btn-primary:hover {
            background: linear-gradient(135deg, #764ba2 0%, #667eea 100%);
        }
        .player-item {
            background: #f8f9fa;
            padding: 10px;
            border-radius: 5px;
            margin: 5px 0;
            font-family: monospace;
        }
        .command-item {
            background: #e7f3ff;
            padding: 10px;
            border-radius: 5px;
            margin: 5px 0;
            display: flex;
            justify-content: space-between;
            align-items: center;
        }
        .status-online { color: #28a745; font-weight: bold; }
        .status-offline { color: #dc3545; font-weight: bold; }
        h1 {
            color: white;
            margin-bottom: 30px;
            text-shadow: 2px 2px 4px rgba(0,0,0,0.3);
        }
    </style>
</head>
<body>
    <div class="container">
        <h1>🎮 Bedrock Server Control Panel</h1>
        
        <div class="row">
            <!-- Player Coordinates -->
            <div class="col-lg-6">
                <div class="card">
                    <div class="card-header">
                        📍 Live Player Coordinates
                    </div>
                    <div class="card-body">
                        <div id="playersList">Loading players...</div>
                        <button class="btn btn-primary btn-sm mt-2" onclick="refreshPlayers()">
                            🔄 Refresh
                        </button>
                    </div>
                </div>
            </div>

            <!-- Custom Commands -->
            <div class="col-lg-6">
                <div class="card">
                    <div class="card-header">
                        ⚙️ Custom Commands
                    </div>
                    <div class="card-body">
                        <div class="input-group mb-2">
                            <input type="text" id="commandName" class="form-control" placeholder="Command name">
                            <input type="text" id="commandText" class="form-control" placeholder="Command text">
                            <button class="btn btn-success" onclick="addCustomCommand()">Add</button>
                        </div>
                        <div id="customCommandsList"></div>
                    </div>
                </div>
            </div>
        </div>

        <!-- Time & Weather Controls -->
        <div class="card">
            <div class="card-header">⏰ Time & Weather Controls</div>
            <div class="card-body">
                <div class="row">
                    <div class="col-md-3">
                        <button class="btn btn-info w-100" onclick="executeCommand('time set day')">🌅 Set Day</button>
                    </div>
                    <div class="col-md-3">
                        <button class="btn btn-info w-100" onclick="executeCommand('time set night')">🌙 Set Night</button>
                    </div>
                    <div class="col-md-3">
                        <button class="btn btn-info w-100" onclick="executeCommand('weather clear')">☀️ Clear Weather</button>
                    </div>
                    <div class="col-md-3">
                        <button class="btn btn-info w-100" onclick="executeCommand('weather rain')">🌧️ Rain</button>
                    </div>
                </div>
                <div class="row mt-2">
                    <div class="col-md-3">
                        <button class="btn btn-info w-100" onclick="executeCommand('weather thunder')">⛈️ Thunder</button>
                    </div>
                    <div class="col-md-3">
                        <button class="btn btn-warning w-100" onclick="executeCommand('gamerule showcoordinates true')">📍 Show Coords</button>
                    </div>
                    <div class="col-md-3">
                        <button class="btn btn-warning w-100" onclick="executeCommand('gamerule showcoordinates false')">🚫 Hide Coords</button>
                    </div>
                    <div class="col-md-3">
                        <button class="btn btn-warning w-100" onclick="executeCommand('gamerule dayCount 0')">Reset Day Count</button>
                    </div>
                </div>
            </div>
        </div>

        <!-- Player Mode Controls -->
        <div class="card">
            <div class="card-header">👤 Player Mode Controls</div>
            <div class="card-body">
                <div class="row">
                    <div class="col-md-3">
                        <button class="btn btn-success w-100" onclick="executeCommand('gamemode s @a')">🎮 Survival</button>
                    </div>
                    <div class="col-md-3">
                        <button class="btn btn-success w-100" onclick="executeCommand('gamemode c @a')">🔨 Creative</button>
                    </div>
                    <div class="col-md-3">
                        <button class="btn btn-warning w-100" onclick="executeCommand('gamemode a @a')">👻 Adventure</button>
                    </div>
                    <div class="col-md-3">
                        <button class="btn btn-danger w-100" onclick="executeCommand('gamemode sp @a')">📖 Spectator</button>
                    </div>
                </div>
            </div>
        </div>

        <!-- Item & Armor Distribution -->
        <div class="card">
            <div class="card-header">🎁 Items & Armor</div>
            <div class="card-body">
                <div class="row">
                    <div class="col-md-4">
                        <button class="btn btn-secondary w-100" onclick="executeCommand('give @a diamond_pickaxe')">⛏️ Diamond Pickaxe</button>
                    </div>
                    <div class="col-md-4">
                        <button class="btn btn-secondary w-100" onclick="executeCommand('give @a diamond_armor')">🛡️ Diamond Armor</button>
                    </div>
                    <div class="col-md-4">
                        <button class="btn btn-secondary w-100" onclick="executeCommand('give @a diamond_sword')">⚔️ Diamond Sword</button>
                    </div>
                </div>
                <div class="row mt-2">
                    <div class="col-md-4">
                        <button class="btn btn-secondary w-100" onclick="executeCommand('give @a golden_apple 64')">🍎 Golden Apples</button>
                    </div>
                    <div class="col-md-4">
                        <button class="btn btn-secondary w-100" onclick="executeCommand('give @a netherite_pickaxe')">💎 Netherite Pickaxe</button>
                    </div>
                    <div class="col-md-4">
                        <button class="btn btn-secondary w-100" onclick="executeCommand('give @a shield')">🛡️ Shield</button>
                    </div>
                </div>
                <div class="row mt-2">
                    <div class="col-md-6">
                        <button class="btn btn-warning w-100" onclick="executeCommand('give @a enchanted_golden_apple')">✨ Enchanted Golden Apple</button>
                    </div>
                    <div class="col-md-6">
                        <button class="btn btn-warning w-100" onclick="executeCommand('effect @a instant_health 1 10')">❤️ Instant Health</button>
                    </div>
                </div>
            </div>
        </div>

        <!-- Explosion & Effects -->
        <div class="card">
            <div class="card-header">💥 Explosions & Effects</div>
            <div class="card-body">
                <div class="row">
                    <div class="col-md-3">
                        <button class="btn btn-danger w-100" onclick="executeCommand('summon tnt ~ ~ ~')">💣 Spawn TNT</button>
                    </div>
                    <div class="col-md-3">
                        <button class="btn btn-danger w-100" onclick="executeCommand('summon tnt ~ ~ ~ {Fuse: 0}')">💥 Instant TNT</button>
                    </div>
                    <div class="col-md-3">
                        <button class="btn btn-danger w-100" onclick="executeCommand('summon creeper ~ ~ ~ {Fuse: 0}')">👹 Creeper Boom</button>
                    </div>
                    <div class="col-md-3">
                        <button class="btn btn-warning w-100" onclick="executeCommand('effect @a wither 10 1')">☠️ Wither Effect</button>
                    </div>
                </div>
                <div class="row mt-2">
                    <div class="col-md-3">
                        <button class="btn btn-info w-100" onclick="executeCommand('summon fireworks_rocket ~ ~ ~')">🎆 Fireworks</button>
                    </div>
                    <div class="col-md-3">
                        <button class="btn btn-info w-100" onclick="executeCommand('effect @a levitation 5 1')">🎈 Levitation</button>
                    </div>
                    <div class="col-md-3">
                        <button class="btn btn-info w-100" onclick="executeCommand('effect @a speed 30 2')">💨 Speed Boost</button>
                    </div>
                    <div class="col-md-3">
                        <button class="btn btn-info w-100" onclick="executeCommand('effect @a invisibility 60')">👻 Invisibility</button>
                    </div>
                </div>
            </div>
        </div>

        <!-- Utility & Admin -->
        <div class="card">
            <div class="card-header">🔧 Utility & Admin</div>
            <div class="card-body">
                <div class="row">
                    <div class="col-md-4">
                        <button class="btn btn-warning w-100" onclick="executeCommand('fill ~ ~ ~ ~100 ~100 ~100 air')">💨 Clear Area</button>
                    </div>
                    <div class="col-md-4">
                        <button class="btn btn-warning w-100" onclick="executeCommand('kill @a')">💀 Kill All Players</button>
                    </div>
                    <div class="col-md-4">
                        <button class="btn btn-warning w-100" onclick="executeCommand('say Server Message Test')">📣 Say Message</button>
                    </div>
                </div>
                <div class="row mt-2">
                    <div class="col-md-4">
                        <button class="btn btn-info w-100" onclick="executeCommand('gamerule pvp true')">⚔️ Enable PvP</button>
                    </div>
                    <div class="col-md-4">
                        <button class="btn btn-info w-100" onclick="executeCommand('gamerule pvp false')">🚫 Disable PvP</button>
                    </div>
                    <div class="col-md-4">
                        <button class="btn btn-info w-100" onclick="executeCommand('gamerule naturalRegeneration true')">❤️ Enable Regen</button>
                    </div>
                </div>
            </div>
        </div>

        <!-- Response Display -->
        <div class="card">
            <div class="card-header">📊 Command Response</div>
            <div class="card-body">
                <div id="response" style="background: #f8f9fa; padding: 10px; border-radius: 5px; font-family: monospace; min-height: 50px;">
                    Ready...
                </div>
            </div>
        </div>
    </div>

    <script src="https://cdn.jsdelivr.net/npm/bootstrap@5.3.0/dist/js/bootstrap.bundle.min.js"></script>
    <script>
        async function executeCommand(command) {
            try {
                const response = await fetch('/send-command', {
                    method: 'POST',
                    body: command
                });
                const data = await response.json();
                document.getElementById('response').innerText = new Date().toLocaleTimeString() + ' - ' + JSON.stringify(data);
            } catch (error) {
                document.getElementById('response').innerText = 'Error: ' + error.message;
            }
        }

        async function refreshPlayers() {
            try {
                const response = await fetch('/player-coords');
                const data = await response.json();
                let html = '';
                if (data.players && data.players.length > 0) {
                    data.players.forEach(player => {
                        html += '<div class="player-item">';
                        html += '<strong>' + player.name + '</strong><br>';
                        html += 'X: ' + player.x.toFixed(2) + ' Y: ' + player.y.toFixed(2) + ' Z: ' + player.z.toFixed(2);
                        html += '</div>';
                    });
                } else {
                    html = '<div class="text-muted">No players online or unable to fetch coordinates</div>';
                }
                document.getElementById('playersList').innerHTML = html;
            } catch (error) {
                document.getElementById('playersList').innerHTML = '<div class="text-danger">Error: ' + error.message + '</div>';
            }
        }

        async function addCustomCommand() {
            const name = document.getElementById('commandName').value;
            const command = document.getElementById('commandText').value;
            
            if (!name || !command) {
                alert('Please enter both name and command');
                return;
            }

            try {
                const response = await fetch('/add-custom-command', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ name: name, command: command })
                });
                const data = await response.json();
                document.getElementById('commandName').value = '';
                document.getElementById('commandText').value = '';
                loadCustomCommands();
            } catch (error) {
                alert('Error: ' + error.message);
            }
        }

        async function loadCustomCommands() {
            try {
                const response = await fetch('/get-custom-commands');
                const data = await response.json();
                let html = '';
                if (data.commands && data.commands.length > 0) {
                    data.commands.forEach((cmd, index) => {
                        html += '<div class="command-item">';
                        html += '<div><strong>' + cmd.name + '</strong><br><small>' + cmd.command + '</small></div>';
                        html += '<button class="btn btn-sm btn-primary" onclick="executeCustom(' + index + ')">Run</button>';
                        html += '<button class="btn btn-sm btn-danger" onclick="deleteCustom(' + index + ')">Del</button>';
                        html += '</div>';
                    });
                } else {
                    html = '<div class="text-muted">No custom commands yet</div>';
                }
                document.getElementById('customCommandsList').innerHTML = html;
            } catch (error) {
                console.error('Error loading custom commands:', error);
            }
        }

        async function executeCustom(index) {
            try {
                const response = await fetch('/execute-custom-command/' + index, {
                    method: 'POST'
                });
                const data = await response.json();
                document.getElementById('response').innerText = new Date().toLocaleTimeString() + ' - ' + JSON.stringify(data);
            } catch (error) {
                document.getElementById('response').innerText = 'Error: ' + error.message;
            }
        }

        async function deleteCustom(index) {
            try {
                await fetch('/delete-custom-command/' + index, {
                    method: 'POST'
                });
                loadCustomCommands();
            } catch (error) {
                alert('Error: ' + error.message);
            }
        }

        // Auto-refresh players every 5 seconds
        setInterval(refreshPlayers, 5000);
        refreshPlayers();
        loadCustomCommands();
    </script>
</body>
</html>`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, html)
}

// playerCoordsHandler returns approximate player coordinates (simulated)
func playerCoordsHandler(w http.ResponseWriter, r *http.Request) {
	// In a real implementation, you'd read this from world data
	// For now, return mock data
	players := []PlayerCoords{
		{Name: "Player1", X: 100.5, Y: 64.0, Z: -50.3},
		{Name: "Player2", X: 200.2, Y: 72.5, Z: 150.8},
	}
	writeJSONResponse(w, http.StatusOK, map[string]interface{}{"players": players})
}

// addCustomCommandHandler adds a custom command
func addCustomCommandHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	var req CustomCommand
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "Invalid request")
		return
	}
	req.CreatedAt = time.Now()
	
	commandsMutex.Lock()
	customCommands = append(customCommands, req)
	commandsMutex.Unlock()
	
	writeJSONResponse(w, http.StatusOK, map[string]string{"message": "Custom command added"})
}

// getCustomCommandsHandler returns all custom commands
func getCustomCommandsHandler(w http.ResponseWriter, r *http.Request) {
	commandsMutex.RLock()
	defer commandsMutex.RUnlock()
	
	writeJSONResponse(w, http.StatusOK, map[string]interface{}{"commands": customCommands})
}

// executeCustomCommandHandler executes a custom command by index
func executeCustomCommandHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	
	indexStr := strings.TrimPrefix(r.URL.Path, "/execute-custom-command/")
	var index int
	if _, err := fmt.Sscanf(indexStr, "%d", &index); err != nil {
		writeJSONError(w, http.StatusBadRequest, "Invalid index")
		return
	}
	
	commandsMutex.Lock()
	if index < 0 || index >= len(customCommands) {
		commandsMutex.Unlock()
		writeJSONError(w, http.StatusNotFound, "Command not found")
		return
	}
	customCommands[index].ExecutedAt = time.Now()
	cmd := customCommands[index]
	commandsMutex.Unlock()
	
	// Execute the command
	fifo, err := os.OpenFile(fifoPath, os.O_WRONLY, 0)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Failed to execute command")
		return
	}
	defer fifo.Close()
	
	_, err = fifo.Write([]byte(cmd.Command + "\n"))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Failed to execute command")
		return
	}
	
	writeJSONResponse(w, http.StatusOK, map[string]string{"message": "Custom command executed: " + cmd.Command})
}

// deleteCustomCommandHandler deletes a custom command by index
func deleteCustomCommandHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	
	indexStr := strings.TrimPrefix(r.URL.Path, "/delete-custom-command/")
	var index int
	if _, err := fmt.Sscanf(indexStr, "%d", &index); err != nil {
		writeJSONError(w, http.StatusBadRequest, "Invalid index")
		return
	}
	
	commandsMutex.Lock()
	if index < 0 || index >= len(customCommands) {
		commandsMutex.Unlock()
		writeJSONError(w, http.StatusNotFound, "Command not found")
		return
	}
	customCommands = append(customCommands[:index], customCommands[index+1:]...)
	commandsMutex.Unlock()
	
	writeJSONResponse(w, http.StatusOK, map[string]string{"message": "Custom command deleted"})
}

func main() {
	// Initialize archive directories
	if err := ensureArchiveDirectories(); err != nil {
		log.Fatalf("Failed to initialize archive directories: %v", err)
	}

	// Restore deleted packs on startup
	if err := restoreDeletedPacks(); err != nil {
		log.Printf("Error during pack restoration: %v", err)
	}

	http.HandleFunc("/", uiHandler)
	http.HandleFunc("/send-command", sendCommandHandler)
	http.HandleFunc("/list-addons", listAddonsHandler)
	http.HandleFunc("/upload-mcaddon", uploadMcAddonHandler)
	http.HandleFunc("/active-addons", activeAddonsHandler)
	http.HandleFunc("/player-coords", playerCoordsHandler)
	http.HandleFunc("/add-custom-command", addCustomCommandHandler)
	http.HandleFunc("/get-custom-commands", getCustomCommandsHandler)
	http.HandleFunc("/execute-custom-command/", executeCustomCommandHandler)
	http.HandleFunc("/delete-custom-command/", deleteCustomCommandHandler)

	port := "8080"
	log.Printf("Starting sidecar command server on port %s...", port)
	log.Printf("Web UI available at http://localhost:%s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
