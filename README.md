# Inkraft 

> Fault-tolerant distributed drawing board built on a Mini-RAFT consensus protocol.

![Go](https://img.shields.io/badge/Go-1.22-00ADD8?style=flat&logo=go)
![Docker](https://img.shields.io/badge/Docker-Compose-2496ED?style=flat&logo=docker)
![WebSocket](https://img.shields.io/badge/WebSocket-Real--Time-green?style=flat)

## What it does
Multiple users draw on a shared canvas in real-time. The backend is a **3-node RAFT cluster** — if any node dies, the system elects a new leader and keeps running. Zero downtime. Zero data loss.

## Architecture
```
Browser ──WS──▶ Gateway :9000 ──HTTP──▶ Leader Replica
                    ▲                        │
                    │                   AppendEntries
                    │                   ▼         ▼
                    └──── broadcast ── R2 :8002  R3 :8003
```

## Key Concepts Implemented
| Feature | Detail |
|---|---|
| Leader Election | Randomized timeout 500–800ms, majority vote |
| Log Replication | AppendEntries RPC, majority commit |
| Catch-Up Sync | Restarted node pulls missing entries via `/sync-log` |
| Zero Downtime | Hot-reload via `air` — edit file → container restarts → system stays live |
| Real-Time | WebSocket broadcast to all clients on commit |

## Run it
```bash
git clone https://github.com/yourname/inkraft
cd inkraft
docker compose up
# Open http://localhost:9000
```

## Stress Test
```bash
# Kill the leader mid-draw
docker compose restart replica1
# System elects new leader in <1s. Canvas preserved.
```

## Stack
Go · Docker · WebSockets · Mini-RAFT · `air` hot-reload