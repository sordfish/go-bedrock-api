package main

import (
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"
)

const fifoPath = "/shared/command_fifo"

// sendCommandHandler reads the command from the POST body and writes it to the FIFO.
func sendCommandHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// Read the command from the request body.
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

	// Open the FIFO file for writing.
	fifo, err := os.OpenFile(fifoPath, os.O_WRONLY, 0)
	if err != nil {
		log.Printf("Error opening FIFO file: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer fifo.Close()

	// Write the command (with newline) to the FIFO.
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

func main() {
	http.HandleFunc("/send-command", sendCommandHandler)

	port := "8080"
	log.Printf("Starting sidecar command server on port %s...", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
