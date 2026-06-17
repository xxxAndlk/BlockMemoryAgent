package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// TUIBroadcaster SSE 广播器
type TUIBroadcaster struct {
	mu      sync.RWMutex
	clients map[string][]*Client // topic_id -> clients
}

// Client SSE 客户端连接
type Client struct {
	TopicID string
	Ch      chan types.UIEvent
	Done    chan struct{}
}

// NewTUIBroadcaster 创建广播器
func NewTUIBroadcaster() *TUIBroadcaster {
	return &TUIBroadcaster{
		clients: make(map[string][]*Client),
	}
}

// Subscribe 订阅主题事件
func (b *TUIBroadcaster) Subscribe(topicID string) *Client {
	client := &Client{
		TopicID: topicID,
		Ch:      make(chan types.UIEvent, 100),
		Done:    make(chan struct{}),
	}
	b.mu.Lock()
	b.clients[topicID] = append(b.clients[topicID], client)
	b.mu.Unlock()
	return client
}

// Unsubscribe 取消订阅
func (b *TUIBroadcaster) Unsubscribe(client *Client) {
	close(client.Done)
	b.mu.Lock()
	defer b.mu.Unlock()
	clients := b.clients[client.TopicID]
	for i, c := range clients {
		if c == client {
			b.clients[client.TopicID] = append(clients[:i], clients[i+1:]...)
			break
		}
	}
	if len(b.clients[client.TopicID]) == 0 {
		delete(b.clients, client.TopicID)
	}
}

// Broadcast 广播事件到所有订阅者
func (b *TUIBroadcaster) Broadcast(topicID string, ev types.UIEvent) {
	b.mu.RLock()
	clients := b.clients[topicID]
	b.mu.RUnlock()

	for _, client := range clients {
		select {
		case client.Ch <- ev:
		default: // 客户端阻塞则丢弃，防背压
		}
	}
}

// BroadcastAgentStatus 广播 Agent 状态
func (b *TUIBroadcaster) BroadcastAgentStatus(topicID string, payload types.AgentStatusPayload) {
	b.Broadcast(topicID, types.UIEvent{
		Type:      "agent.status",
		Timestamp: time.Now(),
		Payload:   payload,
	})
}

// BroadcastGraphStep 广播 Graph 步骤
func (b *TUIBroadcaster) BroadcastGraphStep(topicID string, payload types.GraphStepPayload) {
	b.Broadcast(topicID, types.UIEvent{
		Type:      "graph.step",
		Timestamp: time.Now(),
		Payload:   payload,
	})
}

// BroadcastEpisode 广播 Episode
func (b *TUIBroadcaster) BroadcastEpisode(topicID string, payload types.EpisodePayload) {
	b.Broadcast(topicID, types.UIEvent{
		Type:      "episode.new",
		Timestamp: time.Now(),
		Payload:   payload,
	})
}

// BroadcastEvent 广播 Event
func (b *TUIBroadcaster) BroadcastEvent(topicID string, payload types.EventPayload) {
	b.Broadcast(topicID, types.UIEvent{
		Type:      "workspace.event",
		Timestamp: time.Now(),
		Payload:   payload,
	})
}

// BroadcastStats 广播统计
func (b *TUIBroadcaster) BroadcastStats(topicID string, payload types.StatsView) {
	b.Broadcast(topicID, types.UIEvent{
		Type:      "stats.tick",
		Timestamp: time.Now(),
		Payload:   payload,
	})
}

// SSEHandler HTTP SSE 处理器
func (b *TUIBroadcaster) SSEHandler(w http.ResponseWriter, r *http.Request) {
	topicID := r.URL.Query().Get("topic_id")
	if topicID == "" {
		http.Error(w, "topic_id required", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	client := b.Subscribe(topicID)
	defer b.Unsubscribe(client)

	// 发送连接成功事件
	fmt.Fprintf(w, "data: %s\n\n", `{"type":"connected","timestamp":"`+time.Now().Format(time.RFC3339)+`"}`)
	flusher.Flush()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case ev := <-client.Ch:
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()

		case <-ticker.C:
			// 发送心跳
			fmt.Fprintf(w, ":heartbeat\n\n")
			flusher.Flush()

		case <-client.Done:
			return

		case <-r.Context().Done():
			return
		}
	}
}
