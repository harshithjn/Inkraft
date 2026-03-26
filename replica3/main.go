package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"github.com/harshithj/inkraft/replica3/raft"
)

var gatewayURL string

func main() {
	replicaID := os.Getenv("REPLICA_ID")
	port := os.Getenv("PORT")
	peersEnv := os.Getenv("PEERS")
	gatewayURL = os.Getenv("GATEWAY_URL")

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

	http.HandleFunc("/append-entries", func(w http.ResponseWriter, r *http.Request) {
		var req raft.AppendEntriesRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		raftNode.Mutex.Lock()

		if req.Term < raftNode.CurrentTerm {
			resp := raft.AppendEntriesResponse{Term: raftNode.CurrentTerm, Success: false, LogLength: len(raftNode.Log)}
			raftNode.Mutex.Unlock()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}

		if req.Term > raftNode.CurrentTerm {
			raftNode.Mutex.Unlock()
			raftNode.BecomeFollower(req.Term)
			raftNode.Mutex.Lock()
		}

		raftNode.LeaderID = req.LeaderID

		raftNode.Mutex.Unlock()
		raftNode.StartElectionTimer()
		raftNode.Mutex.Lock()

		if req.PrevLogIndex >= 0 {
			if req.PrevLogIndex >= len(raftNode.Log) || raftNode.Log[req.PrevLogIndex].Term != req.PrevLogTerm {
				resp := raft.AppendEntriesResponse{Term: raftNode.CurrentTerm, Success: false, LogLength: len(raftNode.Log)}
				raftNode.Mutex.Unlock()
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(resp)

				// Async catch-up
				go func(leaderID string, logLength int) {
					var leaderURL string
					raftNode.Mutex.RLock()
					for _, p := range raftNode.Peers {
						if strings.Contains(p, leaderID) {
							leaderURL = p
							break
						}
					}
					raftNode.Mutex.RUnlock()

					if leaderURL != "" {
						syncReq := raft.SyncLogRequest{FromIndex: logLength}
						body, _ := json.Marshal(syncReq)
						resp, err := http.Post(leaderURL+"/sync-log", "application/json", bytes.NewBuffer(body))
						if err == nil {
							defer resp.Body.Close()
							var syncResp raft.SyncLogResponse
							if json.NewDecoder(resp.Body).Decode(&syncResp) == nil {
								if len(syncResp.Entries) > 0 {
									raftNode.Mutex.Lock()
									for _, entry := range syncResp.Entries {
										if entry.Index < len(raftNode.Log) {
											raftNode.Log[entry.Index] = entry
										} else if entry.Index == len(raftNode.Log) {
											raftNode.Log = append(raftNode.Log, entry)
										}
									}
									raftNode.Mutex.Unlock()
								}
							}
						}
					}
				}(req.LeaderID, len(raftNode.Log))
				return
			}
		}

		insertIndex := req.PrevLogIndex + 1
		for i, entry := range req.Entries {
			logIndex := insertIndex + i
			if logIndex < len(raftNode.Log) {
				if raftNode.Log[logIndex].Term != entry.Term {
					raftNode.Log = raftNode.Log[:logIndex]
					raftNode.Log = append(raftNode.Log, entry)
					fmt.Printf("[%s] appended entry index=%d term=%d\n", raftNode.ID, entry.Index, entry.Term)
				}
			} else {
				raftNode.Log = append(raftNode.Log, entry)
				fmt.Printf("[%s] appended entry index=%d term=%d\n", raftNode.ID, entry.Index, entry.Term)
			}
		}

		if req.LeaderCommit > raftNode.CommitIndex {
			lastNewIndex := -1
			if len(req.Entries) > 0 {
				lastNewIndex = req.Entries[len(req.Entries)-1].Index
			} else if len(raftNode.Log) > 0 {
				lastNewIndex = raftNode.Log[len(raftNode.Log)-1].Index
			}

			if req.LeaderCommit < lastNewIndex || lastNewIndex == -1 {
				raftNode.CommitIndex = req.LeaderCommit
			} else {
				raftNode.CommitIndex = lastNewIndex
			}
		}

		resp := raft.AppendEntriesResponse{Term: raftNode.CurrentTerm, Success: true, LogLength: len(raftNode.Log)}
		raftNode.Mutex.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	http.HandleFunc("/sync-log", func(w http.ResponseWriter, r *http.Request) {
		var req raft.SyncLogRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		raftNode.Mutex.RLock()
		entries := make([]raft.LogEntry, 0)
		for i := req.FromIndex; i <= raftNode.CommitIndex && i < len(raftNode.Log); i++ {
			if i >= 0 {
				entries = append(entries, raftNode.Log[i])
			}
		}
		raftNode.Mutex.RUnlock()

		fmt.Printf("[%s] sync-log: sending %d entries from index %d\n", raftNode.ID, len(entries), req.FromIndex)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(raft.SyncLogResponse{Entries: entries})
	})

	http.HandleFunc("/submit-stroke", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var stroke raft.StrokeData
		if err := json.NewDecoder(r.Body).Decode(&stroke); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		raftNode.Mutex.Lock()
		if raftNode.State != raft.Leader {
			raftNode.Mutex.Unlock()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(raft.SubmitStrokeResponse{Success: false, Error: "not leader"})
			return
		}

		entry := raft.LogEntry{
			Index:  len(raftNode.Log),
			Term:   raftNode.CurrentTerm,
			Stroke: stroke,
		}
		raftNode.Log = append(raftNode.Log, entry)
		reqIndex := entry.Index
		reqTerm := entry.Term
		peers := raftNode.Peers
		leaderID := raftNode.ID
		commitIndex := raftNode.CommitIndex
		prevLogIndex := reqIndex - 1
		prevLogTerm := 0
		if prevLogIndex >= 0 && prevLogIndex < len(raftNode.Log) {
			prevLogTerm = raftNode.Log[prevLogIndex].Term
		}
		raftNode.Mutex.Unlock()

		appendReq := raft.AppendEntriesRequest{
			Term:         reqTerm,
			LeaderID:     leaderID,
			PrevLogIndex: prevLogIndex,
			PrevLogTerm:  prevLogTerm,
			Entries:      []raft.LogEntry{entry},
			LeaderCommit: commitIndex,
		}

		reqBody, _ := json.Marshal(appendReq)
		
		var wg sync.WaitGroup
		successAcks := 1 // self ack
		var ackMutex sync.Mutex

		for _, peer := range peers {
			wg.Add(1)
			go func(p string) {
				defer wg.Done()
				client := &http.Client{Timeout: 300 * time.Millisecond}
				resp, err := client.Post(p+"/append-entries", "application/json", bytes.NewBuffer(reqBody))
				if err == nil {
					defer resp.Body.Close()
					var appResp raft.AppendEntriesResponse
					if json.NewDecoder(resp.Body).Decode(&appResp) == nil {
						if appResp.Success {
							ackMutex.Lock()
							successAcks++
							ackMutex.Unlock()
						}
					}
				}
			}(peer)
		}

		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(300 * time.Millisecond):
		}

		ackMutex.Lock()
		acks := successAcks
		ackMutex.Unlock()

		if acks >= (len(peers)+1)/2+1 {
			raftNode.Mutex.Lock()
			if reqIndex > raftNode.CommitIndex {
				raftNode.CommitIndex = reqIndex
				fmt.Printf("[%s] committed index=%d (majority ack)\n", raftNode.ID, reqIndex)
			}
			raftNode.Mutex.Unlock()

			// Notify gateway of committed stroke
			if gatewayURL != "" {
				go func() {
					notifyBody, _ := json.Marshal(entry)
					client := &http.Client{Timeout: 1 * time.Second}
					resp, err := client.Post(gatewayURL+"/notify-gateway", "application/json", bytes.NewBuffer(notifyBody))
					if err != nil {
						fmt.Printf("[%s] failed to notify gateway: %v\n", raftNode.ID, err)
						return
					}
					defer resp.Body.Close()
					fmt.Printf("[%s] notified gateway of commit index=%d\n", raftNode.ID, reqIndex)
				}()
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(raft.SubmitStrokeResponse{Success: true, Index: reqIndex})
		} else {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(raft.SubmitStrokeResponse{Success: false, Error: "failed to get majority ack"})
		}
	})

	raftNode.Start()

	fmt.Printf("Replica %s started on port %s, peers: %s\n", replicaID, port, peersEnv)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
