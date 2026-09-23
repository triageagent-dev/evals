package api

import (
	"encoding/json"
	"net/http"
	"sync"
)

// sseHub fans out events to every connected /stream/ui-updates client.
// Ported from ws_server.py's sse_queues/register_sse_client/
// unregister_sse_client/broadcast_to_ui (the UI's live-update channel is
// SSE, not the /ws/traces WebSocket - that endpoint is the AgentEvals SDK's
// alternate ingestion channel, not yet ported; see README.md).
type sseHub struct {
	mu      sync.Mutex
	clients map[chan any]struct{}
}

func newSSEHub() *sseHub {
	return &sseHub{clients: map[chan any]struct{}{}}
}

func (h *sseHub) register() chan any {
	ch := make(chan any, 64)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *sseHub) unregister(ch chan any) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
	close(ch)
}

// broadcast sends event to every connected client. Ported from
// broadcast_to_ui. A client whose buffer is full is skipped rather than
// blocking the ingest path, since a slow UI client should never stall
// trace ingestion.
func (h *sseHub) broadcast(event any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- event:
		default:
		}
	}
}

// uiUpdatesHandler implements GET /stream/ui-updates: a standard
// text/event-stream SSE endpoint the UI subscribes to via EventSource.
// Ported from api/app.py's ui_updates_stream.
func uiUpdatesHandler(hub *sseHub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		ch := hub.register()
		defer hub.unregister(ch)

		for {
			select {
			case event, ok := <-ch:
				if !ok {
					return
				}
				data, err := json.Marshal(event)
				if err != nil {
					continue
				}
				if _, err := w.Write([]byte("data: ")); err != nil {
					return
				}
				if _, err := w.Write(data); err != nil {
					return
				}
				if _, err := w.Write([]byte("\n\n")); err != nil {
					return
				}
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	}
}
