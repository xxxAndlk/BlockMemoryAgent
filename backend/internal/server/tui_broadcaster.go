package server

import (
	"encoding/json" // 事件 JSON 序列化
	"fmt"           // SSE 帧 fmt.Fprintf
	"net/http"      // HTTP / SSE 处理器
	"sync"          // 读写锁保护 clients 映射
	"time"          // 时间戳与心跳 ticker

	"github.com/blockmemory/agent/backend/pkg/types" // UIEvent 等共享类型
)

// TUIBroadcaster 是面向 TUI / Web 前端的事件广播器。
// 维护 topic_id -> 订阅客户端列表的映射，并发安全（RWMutex 保护）。
type TUIBroadcaster struct {
	mu      sync.RWMutex         // 读写锁，保护 clients 并发访问
	clients map[string][]*Client // topic_id -> clients 按 topic 分桶
}

// Client 表示一个 SSE 客户端连接，持有事件缓冲通道与关闭信号。
type Client struct {
	TopicID string             // 客户端订阅的 topic（一般是 session ID）
	Ch      chan types.UIEvent // 事件缓冲通道（容量 100，背压时丢弃）
	Done    chan struct{}      // 关闭信号，Unsubscribe 时关闭
}

// NewTUIBroadcaster 创建并返回一个新的广播器实例。
// 返回值：*TUIBroadcaster，clients 映射已初始化。
func NewTUIBroadcaster() *TUIBroadcaster {
	return &TUIBroadcaster{
		clients: make(map[string][]*Client), // 初始化 topic -> 客户端列表
	}
}

// Subscribe 为指定 topic 注册一个新的 SSE 客户端。
// 职责：创建带缓冲通道的 Client，追加到 clients[topicID]。
// 参数：topicID - 订阅的主题 ID。
// 返回值：*Client - 订阅者句柄，用于后续接收事件与取消订阅。
// 并发安全：通过 mu.Lock 保护。
func (b *TUIBroadcaster) Subscribe(topicID string) *Client {
	client := &Client{
		TopicID: topicID,                       // 绑定 topic
		Ch:      make(chan types.UIEvent, 100), // 缓冲 100，避免慢客户端阻塞广播
		Done:    make(chan struct{}),           // 用于 Unsubscribe 时通知 SSE 循环退出
	}
	b.mu.Lock()
	b.clients[topicID] = append(b.clients[topicID], client) // 追加到 topic 客户端列表
	b.mu.Unlock()
	return client
}

// Unsubscribe 取消订阅并移除客户端。
// 职责：关闭 Done 信号，从 clients 列表中删除该 Client；若 topic 列表清空则删除映射键。
// 参数：client - 待移除的订阅者。
// 副作用：close(client.Done) 触发 SSEHandler 退出 select 循环。
// 并发安全：通过 mu.Lock 保护列表修改。
func (b *TUIBroadcaster) Unsubscribe(client *Client) {
	close(client.Done) // 通知 SSEHandler 退出
	b.mu.Lock()
	defer b.mu.Unlock()
	clients := b.clients[client.TopicID] // 取出 topic 下所有客户端
	for i, c := range clients {
		if c == client { // 找到要移除的客户端
			b.clients[client.TopicID] = append(clients[:i], clients[i+1:]...) // 切片删除
			break
		}
	}
	if len(b.clients[client.TopicID]) == 0 { // topic 列表空了
		delete(b.clients, client.TopicID) // 清理映射键，避免空切片累积
	}
}

// Broadcast 向指定 topic 的所有订阅者广播一个事件。
// 职责：非阻塞地把事件塞入每个客户端的缓冲通道；缓冲满则丢弃，防止慢客户端背压。
// 参数：topicID - 目标 topic；ev - 待广播事件。
// 并发安全：使用 RLock 读取客户端列表快照后再投递，避免长时间持锁。
func (b *TUIBroadcaster) Broadcast(topicID string, ev types.UIEvent) {
	b.mu.RLock()
	clients := b.clients[topicID] // 快照当前 topic 的客户端列表
	b.mu.RUnlock()

	for _, client := range clients {
		select {
		case client.Ch <- ev: // 正常投递
		default: // 客户端阻塞则丢弃，防背压
		}
	}
}

// BroadcastAgentStatus 广播 Agent 状态变化事件（type=agent.status）。
// 参数：topicID - 目标 topic；payload - Agent 状态载荷。
func (b *TUIBroadcaster) BroadcastAgentStatus(topicID string, payload types.AgentStatusPayload) {
	b.Broadcast(topicID, types.UIEvent{
		Type:      "agent.status", // 事件类型
		Timestamp: time.Now(),     // 事件时间戳
		Payload:   payload,        // Agent 状态载荷
	})
}

// BroadcastGraphStep 广播 Graph 执行步骤事件（type=graph.step）。
// 参数：topicID - 目标 topic；payload - Graph 步骤载荷。
func (b *TUIBroadcaster) BroadcastGraphStep(topicID string, payload types.GraphStepPayload) {
	b.Broadcast(topicID, types.UIEvent{
		Type:      "graph.step", // 事件类型
		Timestamp: time.Now(),   // 事件时间戳
		Payload:   payload,      // Graph 步骤载荷
	})
}

// BroadcastEpisode 广播新 Episode 事件（type=episode.new）。
// 参数：topicID - 目标 topic；payload - Episode 载荷。
func (b *TUIBroadcaster) BroadcastEpisode(topicID string, payload types.EpisodePayload) {
	b.Broadcast(topicID, types.UIEvent{
		Type:      "episode.new", // 事件类型
		Timestamp: time.Now(),    // 事件时间戳
		Payload:   payload,       // Episode 载荷
	})
}

// BroadcastEvent 广播 workspace Event 事件（type=workspace.event）。
// 参数：topicID - 目标 topic；payload - Event 载荷。
func (b *TUIBroadcaster) BroadcastEvent(topicID string, payload types.EventPayload) {
	b.Broadcast(topicID, types.UIEvent{
		Type:      "workspace.event", // 事件类型
		Timestamp: time.Now(),        // 事件时间戳
		Payload:   payload,           // Event 载荷
	})
}

// BroadcastStats 广播统计快照事件（type=stats.tick）。
// 参数：topicID - 目标 topic；payload - 统计视图载荷。
func (b *TUIBroadcaster) BroadcastStats(topicID string, payload types.StatsView) {
	b.Broadcast(topicID, types.UIEvent{
		Type:      "stats.tick", // 事件类型
		Timestamp: time.Now(),   // 事件时间戳
		Payload:   payload,      // 统计视图载荷
	})
}

// SSEHandler 处理 HTTP SSE 订阅请求，建立长连接并持续推送事件。
// 路由：通常挂在 /api/events 或类似 SSE 端点。
// 职责：解析 topic_id，设置 SSE 响应头，订阅广播器，循环把事件 / 心跳 / 关闭信号写入响应。
// 参数：w - HTTP 响应；r - HTTP 请求。
// 副作用：阻塞当前 goroutine 直到客户端断开或 Unsubscribe。
func (b *TUIBroadcaster) SSEHandler(w http.ResponseWriter, r *http.Request) {
	topicID := r.URL.Query().Get("topic_id") // 从 query 解析 topic_id
	if topicID == "" {
		http.Error(w, "topic_id required", http.StatusBadRequest) // 缺参报 400
		return
	}

	w.Header().Set("Content-Type", "text/event-stream") // SSE 协议头
	w.Header().Set("Cache-Control", "no-cache")         // 禁用缓存
	w.Header().Set("Connection", "keep-alive")          // 保持长连接

	flusher, ok := w.(http.Flusher) // 断言 Flusher 接口
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError) // 不支持流式响应
		return
	}

	client := b.Subscribe(topicID) // 订阅 topic
	defer b.Unsubscribe(client)    // 函数退出时取消订阅

	// 发送连接成功事件，让前端知道 SSE 已建立
	fmt.Fprintf(w, "data: %s\n\n", `{"type":"connected","timestamp":"`+time.Now().Format(time.RFC3339)+`"}`)
	flusher.Flush()

	ticker := time.NewTicker(30 * time.Second) // 30 秒心跳，避免代理超时断连
	defer ticker.Stop()

	for {
		select {
		case ev := <-client.Ch: // 收到广播事件
			data, err := json.Marshal(ev) // 序列化为 JSON
			if err != nil {
				continue // 序列化失败跳过，不中断连接
			}
			fmt.Fprintf(w, "data: %s\n\n", data) // SSE 帧格式
			flusher.Flush()                      // 立即推送

		case <-ticker.C: // 30 秒心跳
			// 发送心跳（SSE 注释行，前端忽略，仅保活）
			fmt.Fprintf(w, ":heartbeat\n\n")
			flusher.Flush()

		case <-client.Done: // Unsubscribe 触发，退出
			return

		case <-r.Context().Done(): // 客户端断开，退出
			return
		}
	}
}
