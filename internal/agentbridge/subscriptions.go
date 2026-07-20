package agentbridge

import "fmt"

func (b *Bridge) Subscribe() (string, <-chan Event) {
	id := fmt.Sprintf("subscriber-%d", b.subCounter.Add(1))
	ch := make(chan Event, 128)
	b.mu.Lock()
	b.subscribers[id] = ch
	b.mu.Unlock()
	return id, ch
}

func (b *Bridge) Unsubscribe(id string) {
	b.mu.Lock()
	delete(b.subscribers, id)
	b.mu.Unlock()
}

func (b *Bridge) broadcast(event Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subscribers {
		select {
		case ch <- event:
		default:
		}
	}
}

func (b *Bridge) broadcastStatus() {
	status := b.Status()
	auto := status.SessionAutoApprove
	b.broadcast(Event{
		Type: "agent_status", SessionID: status.SessionID, Status: status.State,
		Model: status.Model, Error: status.Error, SessionAutoApprove: &auto,
	})
}
