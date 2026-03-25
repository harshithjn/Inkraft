package raft

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"time"
)

type Node struct {
	ID    string
	State NodeState

	CurrentTerm int
	VotedFor    string
	Log         []LogEntry
	CommitIndex int

	Peers           []string
	LeaderID        string
	Mutex           sync.RWMutex
	ElectionTimer   *time.Timer
	HeartbeatTicker *time.Ticker
}

func NewNode(id string, peers []string) *Node {
	n := &Node{
		ID:          id,
		State:       Follower,
		CurrentTerm: 0,
		VotedFor:    "",
		Log:         make([]LogEntry, 0),
		CommitIndex: 0,
		Peers:       peers,
		LeaderID:    "",
	}
	return n
}

func (n *Node) Start() {
	n.StartElectionTimer()
}

func (n *Node) StartElectionTimer() {
	n.Mutex.Lock()
	defer n.Mutex.Unlock()

	if n.ElectionTimer != nil {
		n.ElectionTimer.Stop()
	}

	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	timeout := time.Duration(500+r.Intn(300)) * time.Millisecond
	n.ElectionTimer = time.AfterFunc(timeout, func() {
		n.BecomeCandidate()
	})
}

func (n *Node) BecomeFollower(term int) {
	n.Mutex.Lock()
	defer n.Mutex.Unlock()

	oldState := n.State
	n.State = Follower
	n.CurrentTerm = term
	n.VotedFor = ""

	if n.HeartbeatTicker != nil {
		n.HeartbeatTicker.Stop()
	}

	fmt.Printf("[%s] %s->FOLLOWER term=%d\n", n.ID, oldState, n.CurrentTerm)
	
	// Reset election timer
	if n.ElectionTimer != nil {
		n.ElectionTimer.Stop()
	}
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	timeout := time.Duration(500+r.Intn(300)) * time.Millisecond
	n.ElectionTimer = time.AfterFunc(timeout, func() {
		n.BecomeCandidate()
	})
}

func (n *Node) BecomeCandidate() {
	n.Mutex.Lock()
	defer n.Mutex.Unlock()

	oldState := n.State
	n.State = Candidate
	n.CurrentTerm++
	n.VotedFor = n.ID

	fmt.Printf("[%s] %s->CANDIDATE term=%d\n", n.ID, oldState, n.CurrentTerm)

	// Reset election timer for split vote retry
	if n.ElectionTimer != nil {
		n.ElectionTimer.Stop()
	}
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	timeout := time.Duration(500+r.Intn(300)) * time.Millisecond
	fmt.Printf("[%s] Election timer reset for %v (candidate retry)\n", n.ID, timeout)
	n.ElectionTimer = time.AfterFunc(timeout, func() {
		n.BecomeCandidate()
	})

	go n.StartElection()
}

func (n *Node) StartElection() {
	n.Mutex.RLock()
	if n.State != Candidate {
		n.Mutex.RUnlock()
		return
	}
	term := n.CurrentTerm
	id := n.ID
	peers := make([]string, len(n.Peers))
	copy(peers, n.Peers)
	n.Mutex.RUnlock()

	votes := 1 // Vote for self
	var voteMutex sync.Mutex
	var wg sync.WaitGroup

	client := &http.Client{Timeout: 500 * time.Millisecond}

	fmt.Printf("[%s] starting election for term %d\n", id, term)

	for _, peer := range peers {
		wg.Add(1)
		go func(peer string) {
			defer wg.Done()
			req := VoteRequest{
				Term:        term,
				CandidateID: id,
			}
			body, _ := json.Marshal(req)
			resp, err := client.Post(peer+"/request-vote", "application/json", bytes.NewBuffer(body))
			if err != nil {
				fmt.Printf("[%s] error requesting vote from %s: %v\n", id, peer, err)
				return
			}
			defer resp.Body.Close()

			var vResp VoteResponse
			if err := json.NewDecoder(resp.Body).Decode(&vResp); err != nil {
				fmt.Printf("[%s] error decoding vote response from %s: %v\n", id, peer, err)
				return
			}

			if vResp.Term > term {
				fmt.Printf("[%s] found higher term %d from %s, reverting to follower\n", id, vResp.Term, peer)
				n.BecomeFollower(vResp.Term)
				return
			}

			if vResp.Granted {
				fmt.Printf("[%s] vote granted by %s for term %d\n", id, peer, term)
				voteMutex.Lock()
				votes++
				voteMutex.Unlock()
			} else {
				fmt.Printf("[%s] vote denied by %s for term %d (their term: %d)\n", id, peer, term, vResp.Term)
			}
		}(peer)
	}

	// Wait for all votes or timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(300 * time.Millisecond):
	}

	n.Mutex.Lock()
	if n.State == Candidate && n.CurrentTerm == term && votes >= (len(n.Peers)+1)/2+1 {
		fmt.Printf("[%s] CANDIDATE->LEADER term=%d votes=%d\n", n.ID, n.CurrentTerm, votes)
		n.Mutex.Unlock()
		n.BecomeLeader()
	} else {
		n.Mutex.Unlock()
	}
}

func (n *Node) SendHeartbeats() {
	n.Mutex.RLock()
	if n.State != Leader {
		n.Mutex.RUnlock()
		return
	}
	term := n.CurrentTerm
	id := n.ID
	peers := make([]string, len(n.Peers))
	copy(peers, n.Peers)
	n.Mutex.RUnlock()

	client := &http.Client{Timeout: 200 * time.Millisecond}

	for _, peer := range peers {
		go func(peer string) {
			req := HeartbeatRequest{
				Term:     term,
				LeaderID: id,
			}
			body, _ := json.Marshal(req)
			resp, err := client.Post(peer+"/heartbeat", "application/json", bytes.NewBuffer(body))
			if err != nil {
				// Heartbeat failure is common, don't spam too much or log at low frequency
				return
			}
			defer resp.Body.Close()

			var hResp AppendEntriesResponse
			if err := json.NewDecoder(resp.Body).Decode(&hResp); err != nil {
				return
			}

			if hResp.Term > term {
				fmt.Printf("[%s] found higher term %d in heartbeat response from %s\n", id, hResp.Term, peer)
				n.BecomeFollower(hResp.Term)
			}
		}(peer)
	}
}

func (n *Node) BecomeLeader() {
	n.Mutex.Lock()
	defer n.Mutex.Unlock()

	if n.State != Candidate {
		return
	}

	oldState := n.State
	n.State = Leader
	n.LeaderID = n.ID

	if n.ElectionTimer != nil {
		n.ElectionTimer.Stop()
	}

	fmt.Printf("[%s] %s->LEADER term=%d\n", n.ID, oldState, n.CurrentTerm)

	n.HeartbeatTicker = time.NewTicker(150 * time.Millisecond)
	go func() {
		for range n.HeartbeatTicker.C {
			n.SendHeartbeats()
		}
	}()
}
