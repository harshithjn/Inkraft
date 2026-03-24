package raft

func (n *Node) AppendEntry(entry LogEntry) {
	n.Mutex.Lock()
	defer n.Mutex.Unlock()
	n.Log = append(n.Log, entry)
}

func (n *Node) GetEntries(fromIndex int) []LogEntry {
	n.Mutex.RLock()
	defer n.Mutex.RUnlock()
	
	if fromIndex < 0 || fromIndex >= len(n.Log) {
		return []LogEntry{}
	}
	
	entries := make([]LogEntry, len(n.Log)-fromIndex)
	copy(entries, n.Log[fromIndex:])
	return entries
}

func (n *Node) LastLogIndex() int {
	n.Mutex.RLock()
	defer n.Mutex.RUnlock()
	
	if len(n.Log) == 0 {
		return -1
	}
	return n.Log[len(n.Log)-1].Index
}

func (n *Node) LastLogTerm() int {
	n.Mutex.RLock()
	defer n.Mutex.RUnlock()
	
	if len(n.Log) == 0 {
		return 0
	}
	return n.Log[len(n.Log)-1].Term
}
