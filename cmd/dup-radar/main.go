package main

import (
	"log"
	"net/http"

	"github.com/gorilla/mux"
)

func main() {
    r := mux.NewRouter()

    // Set up routes
    r.HandleFunc("/webhook", webhookHandler).Methods("POST")

    // Start the server
    log.Println("Starting server on :8080")
    if err := http.ListenAndServe(":8080", r); err != nil {
        log.Fatalf("Could not start server: %s\n", err)
    }
}

func webhookHandler(w http.ResponseWriter, r *http.Request) {
    // Handle GitHub webhook events
    w.WriteHeader(http.StatusOK)
    w.Write([]byte("Webhook received"))
}
