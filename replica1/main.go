package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
)

func main() {
	replicaID := os.Getenv("REPLICA_ID")
	port := os.Getenv("PORT")
	peers := os.Getenv("PEERS")

	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	http.HandleFunc("/request-vote", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	http.HandleFunc("/append-entries", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	http.HandleFunc("/heartbeat", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	http.HandleFunc("/sync-log", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	fmt.Printf("Replica %s started on port %s, peers: %s\n", replicaID, port, peers)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
