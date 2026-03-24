package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/harshithj/inkraft/replica3/raft"
)

func main() {
	replicaID := os.Getenv("REPLICA_ID")
	port := os.Getenv("PORT")
	peersEnv := os.Getenv("PEERS")

	var peers []string
	if peersEnv != "" {
		peers = strings.Split(peersEnv, ",")
	}

	raftNode := raft.NewNode("replica"+replicaID, peers)

	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		raftNode.Mutex.RLock()
		status := map[string]interface{}{
			"id":          raftNode.ID,
			"state":       string(raftNode.State),
			"term":        raftNode.CurrentTerm,
			"commitIndex": raftNode.CommitIndex,
			"logLength":   len(raftNode.Log),
			"leader":      raftNode.LeaderID,
		}
		raftNode.Mutex.RUnlock()

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(status); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	http.HandleFunc("/request-vote", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	http.HandleFunc("/append-entries", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	http.HandleFunc("/heartbeat", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	http.HandleFunc("/sync-log", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	fmt.Printf("Replica %s started on port %s, peers: %s\n", replicaID, port, peersEnv)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
