package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/harshithj/inkraft/replica1/raft"
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

	http.HandleFunc("/request-vote", func(w http.ResponseWriter, r *http.Request) {
		var req raft.VoteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		fmt.Printf("[%s] received RequestVote from %s term %d\n", raftNode.ID, req.CandidateID, req.Term)

		raftNode.Mutex.Lock()
		if req.Term > raftNode.CurrentTerm {
			raftNode.Mutex.Unlock()
			raftNode.BecomeFollower(req.Term)
			raftNode.Mutex.Lock()
		}

		resp := raft.VoteResponse{
			Term:    raftNode.CurrentTerm,
			Granted: false,
		}

		if req.Term == raftNode.CurrentTerm &&
			(raftNode.VotedFor == "" || raftNode.VotedFor == req.CandidateID) {
			
			lastIndex := -1
			lastTerm := 0
			if len(raftNode.Log) > 0 {
				lastIndex = raftNode.Log[len(raftNode.Log)-1].Index
				lastTerm = raftNode.Log[len(raftNode.Log)-1].Term
			}

			if req.LastLogTerm > lastTerm || (req.LastLogTerm == lastTerm && req.LastLogIndex >= lastIndex) {
				raftNode.VotedFor = req.CandidateID
				resp.Granted = true
				raftNode.Mutex.Unlock()
				raftNode.StartElectionTimer()
			} else {
				raftNode.Mutex.Unlock()
			}
		} else {
			raftNode.Mutex.Unlock()
		}

		fmt.Printf("[%s] responding to RequestVote from %s: granted=%v, term=%d\n", raftNode.ID, req.CandidateID, resp.Granted, resp.Term)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	http.HandleFunc("/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		var req raft.HeartbeatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		raftNode.Mutex.Lock()
		if req.Term > raftNode.CurrentTerm {
			raftNode.Mutex.Unlock()
			raftNode.BecomeFollower(req.Term)
			raftNode.Mutex.Lock()
		} else if req.Term == raftNode.CurrentTerm && raftNode.State != raft.Follower {
			raftNode.Mutex.Unlock()
			raftNode.BecomeFollower(req.Term)
			raftNode.Mutex.Lock()
		}

		resp := raft.AppendEntriesResponse{
			Term:    raftNode.CurrentTerm,
			Success: false,
		}

		if req.Term >= raftNode.CurrentTerm {
			raftNode.LeaderID = req.LeaderID
			fmt.Printf("[%s] received heartbeat from leader=%s term=%d\n", raftNode.ID, req.LeaderID, req.Term)
			raftNode.Mutex.Unlock()
			raftNode.StartElectionTimer()
			resp.Success = true
		} else {
			raftNode.Mutex.Unlock()
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	http.HandleFunc("/append-entries", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	http.HandleFunc("/sync-log", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	raftNode.Start()

	fmt.Printf("Replica %s started on port %s, peers: %s\n", replicaID, port, peersEnv)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
