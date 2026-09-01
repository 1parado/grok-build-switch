package agentloop

import (
	"encoding/json"
	"sync"

	"grok_switch/internal/llm"
)

// EventType 是引擎事件类型。命名对齐现有 agentbridge.Event 的前端协议，
// 尽量让 server 层做最小翻译即可推给浏览器。
type EventType string

const (
	EventTurnBegin     EventType = "turn_begin"
	EventStepBegin     EventType = "step_begin"
	EventStepEnd       EventType = "step_end"
	EventTextDelta     EventType = "text_delta"
	EventThinkDelta    EventType = "think_delta"
	EventToolCall      EventType = "tool_call"
	EventToolResult    EventType = "tool_result"
	EventTurnEnd       EventType = "turn_end"
	EventTurnInterrupt EventType = "turn_interrupted"
	EventUsage         EventType = "usage"
	EventError         EventType = "error"
)

// Event 是引擎的单出口事件。录制器与直播订阅者收到的是同一个值。
type Event struct {
	Type EventType `json:"type"`

	TurnID  string `json:"turn_id,omitempty"`
	Step    int    `json:"step,omitempty"`
	Session string `json:"session,omitempty"`

	// 文本增量 / 完整文本
	Text string `json:"text,omitempty"`
	// 思考增量
	Think string `json:"think,omitempty"`

	// 工具事件
	ToolCall   *ToolCallEvent   `json:"tool,omitempty"`
	ToolResult *ToolResultEvent `json:"tool_result,omitempty"`

	// 结束原因与错误
	StopReason TurnStopReason `json:"stop_reason,omitempty"`
	Error      string         `json:"error,omitempty"`

	// 用量
	Usage *llm.TokenUsage `json:"usage,omitempty"`
	// UsageAccuracy 见 llm.UsageAccuracy
	UsageAccuracy string `json:"usage_accuracy,omitempty"`
}

// ToolCallEvent 描述模型发起的一次工具调用。
type ToolCallEvent struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// ToolResultEvent 描述一次工具执行结果。
type ToolResultEvent struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Output    string `json:"output,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	// Media 携带工具产出的结构化媒体（如 generate_image 的图片引用），
	// 供宿主直接渲染，避免从文本输出做不可靠的路径提取。
	Media []llm.ContentPart `json:"-"`
}

// Dispatcher 是事件单出口。Dispatch 必须非阻塞且不 panic 外泄。
type Dispatcher interface {
	Dispatch(Event)
}

// Broadcaster 是 Dispatcher 的默认实现：录制器 + N 个直播订阅。
// 订阅者慢速时丢弃其事件（带计数），绝不阻塞 loop。
type Broadcaster struct {
	mu        sync.Mutex
	recorders []Dispatcher
	live      map[int]chan Event
	nextID    int
	dropped   int
}

func NewBroadcaster() *Broadcaster {
	return &Broadcaster{live: map[int]chan Event{}}
}

// AddRecorder 注册持久化订阅（不丢事件，同步调用）。
func (b *Broadcaster) AddRecorder(d Dispatcher) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recorders = append(b.recorders, d)
}

// Subscribe 注册直播订阅，返回订阅 ID 与事件 chan。缓冲满即丢事件。
func (b *Broadcaster) Subscribe(buffer int) (int, <-chan Event) {
	if buffer < 16 {
		buffer = 16
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextID
	b.nextID++
	ch := make(chan Event, buffer)
	b.live[id] = ch
	return id, ch
}

// Unsubscribe 注销直播订阅。
func (b *Broadcaster) Unsubscribe(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.live[id]; ok {
		close(ch)
		delete(b.live, id)
	}
}

// Dropped 返回直播丢弃的事件总数（可观测性）。
func (b *Broadcaster) Dropped() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}

func (b *Broadcaster) Dispatch(ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, r := range b.recorders {
		r.Dispatch(ev)
	}
	for _, ch := range b.live {
		select {
		case ch <- ev:
		default:
			b.dropped++
		}
	}
}

// FuncDispatcher 用函数适配 Dispatcher 接口。
type FuncDispatcher func(Event)

func (f FuncDispatcher) Dispatch(ev Event) { f(ev) }
