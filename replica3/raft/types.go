package raft

type NodeState string

const (
	Follower  NodeState = "Follower"
	Candidate NodeState = "Candidate"
	Leader    NodeState = "Leader"
)

type StrokeData struct {
	X0    float64 `json:"x0"`
	Y0    float64 `json:"y0"`
	X1    float64 `json:"x1"`
	Y1    float64 `json:"y1"`
	Color string  `json:"color"`
	Width float64 `json:"width"`
}

type LogEntry struct {
	Index  int        `json:"index"`
	Term   int        `json:"term"`
	Stroke StrokeData `json:"stroke"`
}

type VoteRequest struct {
	Term         int    `json:"term"`
	CandidateID  string `json:"candidateId"`
	LastLogIndex int    `json:"lastLogIndex"`
	LastLogTerm  int    `json:"lastLogTerm"`
}

type VoteResponse struct {
	Term    int  `json:"term"`
	Granted bool `json:"granted"`
}

type AppendEntriesRequest struct {
	Term         int        `json:"term"`
	LeaderID     string     `json:"leaderId"`
	PrevLogIndex int        `json:"prevLogIndex"`
	PrevLogTerm  int        `json:"prevLogTerm"`
	Entries      []LogEntry `json:"entries"`
	LeaderCommit int        `json:"leaderCommit"`
}

type AppendEntriesResponse struct {
	Term      int  `json:"term"`
	Success   bool `json:"success"`
	LogLength int  `json:"logLength"`
}

type HeartbeatRequest struct {
	Term         int    `json:"term"`
	LeaderID     string `json:"leaderId"`
	LeaderCommit int    `json:"leaderCommit"`
}
