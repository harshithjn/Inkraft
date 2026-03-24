package raft

import (
	"fmt"
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
	return &Node{
		ID:          id,
		State:       Follower,
		CurrentTerm: 0,
		VotedFor:    "",
		Log:         make([]LogEntry, 0),
		CommitIndex: 0,
		Peers:       peers,
		LeaderID:    "",
	}
}

func (n *Node) BecomeFollower(term int) {
	n.Mutex.Lock()
	defer n.Mutex.Unlock()

	n.State = Follower
	n.CurrentTerm = term
	n.VotedFor = ""

	fmt.Printf("[%s] term=%d -> FOLLOWER\n", n.ID, n.CurrentTerm)
}

func (n *Node) BecomeCandidate() {
	n.Mutex.Lock()
	defer n.Mutex.Unlock()

	n.State = Candidate
	n.CurrentTerm++
	n.VotedFor = n.ID

	fmt.Printf("[%s] term=%d -> CANDIDATE\n", n.ID, n.CurrentTerm)
}

func (n *Node) BecomeLeader() {
	n.Mutex.Lock()
	defer n.Mutex.Unlock()

	n.State = Leader
	n.LeaderID = n.ID

	fmt.Printf("[%s] term=%d -> LEADER\n", n.ID, n.CurrentTerm)
}
