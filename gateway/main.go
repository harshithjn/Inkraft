package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ---- Types ----

type StrokeMessage struct {
	Type  string  `json:"type"`
	X0    float64 `json:"x0"`
	Y0    float64 `json:"y0"`
	X1    float64 `json:"x1"`
	Y1    float64 `json:"y1"`
	Color string  `json:"color"`
	Width float64 `json:"width"`
	Index int     `json:"index,omitempty"`
}

type SystemMessage struct {
	Type    string `json:"type"`
	Message string `json:"message,omitempty"`
	Leader  string `json:"leader,omitempty"`
}

type HistoryMessage struct {
	Type    string          `json:"type"`
	Strokes []StrokeMessage `json:"strokes"`
}

type IncomingMessage struct {
	Type string `json:"type"`
}

type LogEntry struct {
	Index  int        `json:"index"`
	Term   int        `json:"term"`
	Stroke StrokeData `json:"stroke"`
}

type StrokeData struct {
	X0    float64 `json:"x0"`
	Y0    float64 `json:"y0"`
	X1    float64 `json:"x1"`
	Y1    float64 `json:"y1"`
	Color string  `json:"color"`
	Width float64 `json:"width"`
}

type ReplicaStatus struct {
	ID          string `json:"id"`
	State       string `json:"state"`
	CommitIndex int    `json:"commitIndex"`
	LogLength   int    `json:"logLength"`
}

type SyncLogRequest struct {
	FromIndex int `json:"fromIndex"`
}

type SyncLogResponse struct {
	Entries []LogEntry `json:"entries"`
}

// ---- WebSocket Hub ----

type Client struct {
	conn *websocket.Conn
	send chan []byte
}

type Hub struct {
	clients    map[*Client]bool
	register   chan *Client
	unregister chan *Client
	broadcast  chan []byte
	mu         sync.RWMutex
}

func newHub() *Hub {
	return &Hub{
		clients:    make(map[*Client]bool),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		broadcast:  make(chan []byte, 256),
	}
}

func (h *Hub) run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			h.mu.Unlock()
			log.Printf("[hub] client connected (%d total)", len(h.clients))

		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
			}
			h.mu.Unlock()
			log.Printf("[hub] client disconnected (%d total)", len(h.clients))

		case msg := <-h.broadcast:
			h.mu.RLock()
			for client := range h.clients {
				select {
				case client.send <- msg:
				default:
					go func(c *Client) {
						h.unregister <- c
					}(client)
				}
			}
			h.mu.RUnlock()
		}
	}
}

func (h *Hub) broadcastAll(msg []byte) {
	h.broadcast <- msg
}

// ---- Leader Discovery ----

type LeaderTracker struct {
	replicas  []string
	leaderURL string
	leaderID  string
	mu        sync.RWMutex
	hub       *Hub
}

func newLeaderTracker(replicas []string, hub *Hub) *LeaderTracker {
	return &LeaderTracker{
		replicas: replicas,
		hub:      hub,
	}
}

func (lt *LeaderTracker) getLeader() (string, string) {
	lt.mu.RLock()
	defer lt.mu.RUnlock()
	return lt.leaderURL, lt.leaderID
}

func (lt *LeaderTracker) startPolling() {
	client := &http.Client{Timeout: 400 * time.Millisecond}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		var foundURL, foundID string

		for _, replica := range lt.replicas {
			resp, err := client.Get(replica + "/status")
			if err != nil {
				continue
			}

			var status ReplicaStatus
			if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
				resp.Body.Close()
				continue
			}
			resp.Body.Close()

			if status.State == "Leader" {
				foundURL = replica
				foundID = status.ID
				break
			}
		}

		lt.mu.Lock()
		oldLeader := lt.leaderID
		lt.leaderURL = foundURL
		lt.leaderID = foundID
		lt.mu.Unlock()

		if foundID != oldLeader {
			if foundID != "" {
				log.Printf("[leader] leader changed: %s -> %s (%s)", oldLeader, foundID, foundURL)
				msg, _ := json.Marshal(SystemMessage{
					Type:   "leader_changed",
					Leader: foundID,
				})
				lt.hub.broadcastAll(msg)
			} else if oldLeader != "" {
				log.Printf("[leader] leader lost (was %s)", oldLeader)
				msg, _ := json.Marshal(SystemMessage{
					Type:    "no_leader",
					Message: "Election in progress",
				})
				lt.hub.broadcastAll(msg)
			}
		}
	}
}

// ---- History Fetching ----

func fetchHistory(lt *LeaderTracker) ([]LogEntry, error) {
	leaderURL, _ := lt.getLeader()
	if leaderURL == "" {
		return nil, fmt.Errorf("no leader available")
	}

	// Get leader status to know commit index
	client := &http.Client{Timeout: 2 * time.Second}
	statusResp, err := client.Get(leaderURL + "/status")
	if err != nil {
		return nil, fmt.Errorf("failed to get leader status: %w", err)
	}
	var status ReplicaStatus
	json.NewDecoder(statusResp.Body).Decode(&status)
	statusResp.Body.Close()

	if status.LogLength == 0 {
		return []LogEntry{}, nil
	}

	// Fetch all log entries via /sync-log
	syncReq := SyncLogRequest{FromIndex: 0}
	body, _ := json.Marshal(syncReq)
	resp, err := client.Post(leaderURL+"/sync-log", "application/json", bytes.NewBuffer(body))
	if err != nil {
		return nil, fmt.Errorf("failed to sync log: %w", err)
	}
	defer resp.Body.Close()

	var syncResp SyncLogResponse
	if err := json.NewDecoder(resp.Body).Decode(&syncResp); err != nil {
		return nil, fmt.Errorf("failed to decode sync response: %w", err)
	}

	return syncResp.Entries, nil
}

// ---- Stroke Forwarding ----

func forwardToLeader(lt *LeaderTracker, stroke StrokeMessage) error {
	strokeData := StrokeData{
		X0:    stroke.X0,
		Y0:    stroke.Y0,
		X1:    stroke.X1,
		Y1:    stroke.Y1,
		Color: stroke.Color,
		Width: stroke.Width,
	}

	deadline := time.Now().Add(2 * time.Second)
	client := &http.Client{Timeout: 1 * time.Second}

	for {
		leaderURL, _ := lt.getLeader()
		if leaderURL != "" {
			body, _ := json.Marshal(strokeData)
			resp, err := client.Post(leaderURL+"/submit-stroke", "application/json", bytes.NewBuffer(body))
			if err == nil {
				respBody, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
				log.Printf("[forward] leader returned %d: %s", resp.StatusCode, string(respBody))
			} else {
				log.Printf("[forward] error posting to leader %s: %v", leaderURL, err)
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("no leader available after 2s timeout")
		}

		log.Printf("[forward] no leader, retrying in 200ms...")
		time.Sleep(200 * time.Millisecond)
	}
}

// ---- WebSocket Handler ----

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func handleWebSocket(hub *Hub, lt *LeaderTracker, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[ws] upgrade error: %v", err)
		return
	}

	client := &Client{
		conn: conn,
		send: make(chan []byte, 256),
	}
	hub.register <- client

	// Send connected message
	connMsg, _ := json.Marshal(SystemMessage{
		Type:    "connected",
		Message: "Inkraft connected",
	})
	client.send <- connMsg

	// Send current leader info
	_, leaderID := lt.getLeader()
	if leaderID != "" {
		leaderMsg, _ := json.Marshal(SystemMessage{
			Type:   "leader_changed",
			Leader: leaderID,
		})
		client.send <- leaderMsg
	} else {
		noLeaderMsg, _ := json.Marshal(SystemMessage{
			Type:    "no_leader",
			Message: "Election in progress",
		})
		client.send <- noLeaderMsg
	}

	// Write pump
	go func() {
		defer conn.Close()
		for msg := range client.send {
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		}
	}()

	// Read pump
	go func() {
		defer func() {
			hub.unregister <- client
			conn.Close()
		}()

		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
					log.Printf("[ws] read error: %v", err)
				}
				return
			}

			var incoming IncomingMessage
			if err := json.Unmarshal(message, &incoming); err != nil {
				log.Printf("[ws] invalid message: %v", err)
				continue
			}

			switch incoming.Type {
			case "get_history":
				log.Printf("[ws] client requested history")
				go func() {
					entries, err := fetchHistory(lt)
					if err != nil {
						log.Printf("[ws] history fetch error: %v", err)
						errMsg, _ := json.Marshal(HistoryMessage{
							Type:    "history",
							Strokes: []StrokeMessage{},
						})
						client.send <- errMsg
						return
					}

					strokes := make([]StrokeMessage, len(entries))
					for i, e := range entries {
						strokes[i] = StrokeMessage{
							Type:  "stroke",
							X0:    e.Stroke.X0,
							Y0:    e.Stroke.Y0,
							X1:    e.Stroke.X1,
							Y1:    e.Stroke.Y1,
							Color: e.Stroke.Color,
							Width: e.Stroke.Width,
							Index: e.Index,
						}
					}

					histMsg, _ := json.Marshal(HistoryMessage{
						Type:    "history",
						Strokes: strokes,
					})
					client.send <- histMsg
					log.Printf("[ws] sent %d history strokes", len(strokes))
				}()

			case "stroke":
				var stroke StrokeMessage
				json.Unmarshal(message, &stroke)
				go func() {
					if err := forwardToLeader(lt, stroke); err != nil {
						log.Printf("[ws] forward error: %v", err)
					}
				}()
			}
		}
	}()
}

// ---- Notify Gateway Handler ----

func handleNotifyGateway(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var entry LogEntry
		if err := json.NewDecoder(r.Body).Decode(&entry); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		log.Printf("[notify] committed stroke index=%d", entry.Index)

		msg, _ := json.Marshal(StrokeMessage{
			Type:  "stroke",
			X0:    entry.Stroke.X0,
			Y0:    entry.Stroke.Y0,
			X1:    entry.Stroke.X1,
			Y1:    entry.Stroke.Y1,
			Color: entry.Stroke.Color,
			Width: entry.Stroke.Width,
			Index: entry.Index,
		})
		hub.broadcastAll(msg)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}
}

// ---- History HTTP Handler ----

func handleHistory(lt *LeaderTracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		entries, err := fetchHistory(lt)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}

		strokes := make([]StrokeMessage, len(entries))
		for i, e := range entries {
			strokes[i] = StrokeMessage{
				Type:  "stroke",
				X0:    e.Stroke.X0,
				Y0:    e.Stroke.Y0,
				X1:    e.Stroke.X1,
				Y1:    e.Stroke.Y1,
				Color: e.Stroke.Color,
				Width: e.Stroke.Width,
				Index: e.Index,
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(HistoryMessage{
			Type:    "history",
			Strokes: strokes,
		})
	}
}

// ---- Main ----

func main() {
	replicasEnv := os.Getenv("REPLICAS")
	if replicasEnv == "" {
		replicasEnv = "http://localhost:8001,http://localhost:8002,http://localhost:8003"
	}
	replicas := strings.Split(replicasEnv, ",")

	log.Printf("[gateway] replicas: %v", replicas)

	hub := newHub()
	go hub.run()

	lt := newLeaderTracker(replicas, hub)
	go lt.startPolling()

	// Serve frontend
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.ServeFile(w, r, "/frontend/index.html")
			return
		}
		http.ServeFile(w, r, "/frontend"+r.URL.Path)
	})

	// WebSocket endpoint
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		handleWebSocket(hub, lt, w, r)
	})

	// Health check
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// History endpoint
	http.HandleFunc("/history", handleHistory(lt))

	// Notify gateway (called by leader after commit)
	http.HandleFunc("/notify-gateway", handleNotifyGateway(hub))

	fmt.Println("Gateway started on port 9000")
	log.Fatal(http.ListenAndServe(":9000", nil))
}
