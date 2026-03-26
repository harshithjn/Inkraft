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
	"github.com/harshithj/inkraft/replica1/raft"
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

	// ---- /status ----
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

	// ---- /debug ----
	http.HandleFunc("/debug", func(w http.ResponseWriter, r *http.Request) {
		raftNode.Mutex.RLock()
		lastHB := ""
		if !raftNode.LastHeartbeat.IsZero() {
			lastHB = raftNode.LastHeartbeat.Format(time.RFC3339Nano)
		}
		debug := map[string]interface{}{
			"id":            raftNode.ID,
			"state":         string(raftNode.State),
			"term":          raftNode.CurrentTerm,
			"commitIndex":   raftNode.CommitIndex,
			"logLength":     len(raftNode.Log),
			"leader":        raftNode.LeaderID,
			"peers":         raftNode.Peers,
			"lastHeartbeat": lastHB,
		}
		raftNode.Mutex.RUnlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(debug)
	})

	// ---- /request-vote ----
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

	// ---- /heartbeat ----
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
			raftNode.LastHeartbeat = time.Now()

			// Advance commitIndex if we have the log, else trigger catch-up
			if req.LeaderCommit > raftNode.CommitIndex {
				lastLogIdx := len(raftNode.Log) - 1
				if req.LeaderCommit <= lastLogIdx {
					raftNode.CommitIndex = req.LeaderCommit
					fmt.Printf("[%s] commitIndex advanced to %d via heartbeat\n", raftNode.ID, raftNode.CommitIndex)
				} else {
					// Behind! Trigger catch-up
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
							client := &http.Client{Timeout: 2 * time.Second}
							resp, err := client.Post(leaderURL+"/sync-log", "application/json", bytes.NewBuffer(body))
							if err == nil {
								defer resp.Body.Close()
								var syncResp raft.SyncLogResponse
								if json.NewDecoder(resp.Body).Decode(&syncResp) == nil {
									if len(syncResp.Entries) > 0 {
										raftNode.Mutex.Lock()
										// ... (same sync logic as below)
										for _, entry := range syncResp.Entries {
											if entry.Index < len(raftNode.Log) {
												raftNode.Log[entry.Index] = entry
											} else if entry.Index == len(raftNode.Log) {
												raftNode.Log = append(raftNode.Log, entry)
											}
										}
										if syncResp.CommitIndex > raftNode.CommitIndex {
											lastIdx := len(raftNode.Log) - 1
											if syncResp.CommitIndex <= lastIdx {
												raftNode.CommitIndex = syncResp.CommitIndex
											} else if lastIdx >= 0 {
												raftNode.CommitIndex = lastIdx
											}
										}
										fmt.Printf("[%s] heartbeat catch-up: logLength=%d commitIndex=%d\n", raftNode.ID, len(raftNode.Log), raftNode.CommitIndex)
										raftNode.Mutex.Unlock()
									}
								}
							}
						}
					}(req.LeaderID, len(raftNode.Log))
				}
			}

			raftNode.Mutex.Unlock()
			raftNode.StartElectionTimer()
			resp.Success = true
		} else {
			raftNode.Mutex.Unlock()
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	// ---- /append-entries ----
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
		raftNode.LastHeartbeat = time.Now()

		// Reset election timer (release lock, re-acquire)
		raftNode.Mutex.Unlock()
		raftNode.StartElectionTimer()
		raftNode.Mutex.Lock()

		// Log consistency check
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
						client := &http.Client{Timeout: 2 * time.Second}
						resp, err := client.Post(leaderURL+"/sync-log", "application/json", bytes.NewBuffer(body))
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
									// Update commitIndex from sync response
									if syncResp.CommitIndex > raftNode.CommitIndex {
										lastIdx := len(raftNode.Log) - 1
										if syncResp.CommitIndex <= lastIdx {
											raftNode.CommitIndex = syncResp.CommitIndex
										} else if lastIdx >= 0 {
											raftNode.CommitIndex = lastIdx
										}
									}
									fmt.Printf("[%s] sync-log catch-up: logLength=%d commitIndex=%d\n", raftNode.ID, len(raftNode.Log), raftNode.CommitIndex)
									raftNode.Mutex.Unlock()
								}
							}
						}
					}
				}(req.LeaderID, len(raftNode.Log))
				return
			}
		}

		// Append new entries (idempotent: skip entries already matching)
		insertIndex := req.PrevLogIndex + 1
		for i, entry := range req.Entries {
			logIndex := insertIndex + i
			if logIndex < len(raftNode.Log) {
				if raftNode.Log[logIndex].Term != entry.Term {
					// Conflict: truncate and append
					raftNode.Log = raftNode.Log[:logIndex]
					raftNode.Log = append(raftNode.Log, entry)
					fmt.Printf("[%s] resolved conflict, appended entry index=%d term=%d\n", raftNode.ID, entry.Index, entry.Term)
				}
				// else: entry already exists with same term — skip (idempotent)
			} else if logIndex == len(raftNode.Log) {
				raftNode.Log = append(raftNode.Log, entry)
				fmt.Printf("[%s] appended entry index=%d term=%d\n", raftNode.ID, entry.Index, entry.Term)
			}
			// else: gap — skip (will be filled by sync-log)
		}

		if req.LeaderCommit > raftNode.CommitIndex {
			lastNewIndex := -1
			if len(req.Entries) > 0 {
				lastNewIndex = req.Entries[len(req.Entries)-1].Index
			} else if len(raftNode.Log) > 0 {
				lastNewIndex = raftNode.Log[len(raftNode.Log)-1].Index
			}

			if lastNewIndex >= 0 {
				if req.LeaderCommit < lastNewIndex {
					raftNode.CommitIndex = req.LeaderCommit
				} else {
					raftNode.CommitIndex = lastNewIndex
				}
			}
			fmt.Printf("[%s] commitIndex updated to %d\n", raftNode.ID, raftNode.CommitIndex)
		}

		resp := raft.AppendEntriesResponse{Term: raftNode.CurrentTerm, Success: true, LogLength: len(raftNode.Log)}
		raftNode.Mutex.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	// ---- /sync-log ----
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
		commitIdx := raftNode.CommitIndex
		raftNode.Mutex.RUnlock()

		fmt.Printf("[%s] sync-log: sending %d entries from index %d, commitIndex=%d\n", raftNode.ID, len(entries), req.FromIndex, commitIdx)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(raft.SyncLogResponse{Entries: entries, CommitIndex: commitIdx})
	})

	// ---- /submit-stroke ----
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
				client := &http.Client{Timeout: 500 * time.Millisecond}
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
		case <-time.After(500 * time.Millisecond):
		}

		ackMutex.Lock()
		acks := successAcks
		ackMutex.Unlock()

		quorum := (len(peers)+1)/2 + 1
		if acks >= quorum {
			raftNode.Mutex.Lock()
			if reqIndex > raftNode.CommitIndex {
				raftNode.CommitIndex = reqIndex
				fmt.Printf("[%s] committed index=%d term=%d (majority ack=%d)\n", raftNode.ID, reqIndex, reqTerm, acks)
			}
			raftNode.Mutex.Unlock()

			// Notify gateway of committed stroke
			if gatewayURL != "" {
				go func() {
					notifyBody, _ := json.Marshal(entry)
					client := &http.Client{Timeout: 2 * time.Second}
					for attempt := 0; attempt < 3; attempt++ {
						resp, err := client.Post(gatewayURL+"/notify-gateway", "application/json", bytes.NewBuffer(notifyBody))
						if err != nil {
							fmt.Printf("[%s] failed to notify gateway (attempt %d): %v\n", raftNode.ID, attempt+1, err)
							time.Sleep(200 * time.Millisecond)
							continue
						}
						resp.Body.Close()
						fmt.Printf("[%s] notified gateway of commit index=%d\n", raftNode.ID, reqIndex)
						return
					}
				}()
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(raft.SubmitStrokeResponse{Success: true, Index: reqIndex})
		} else {
			fmt.Printf("[%s] failed to commit index=%d: acks=%d quorum=%d\n", raftNode.ID, reqIndex, acks, quorum)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(raft.SubmitStrokeResponse{Success: false, Error: "failed to get majority ack"})
		}
	})

	raftNode.Start()

	fmt.Printf("Replica %s started on port %s, peers: %s\n", replicaID, port, peersEnv)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
