package chat

import "sync"

// Hub is only a wake-up mechanism. Every SSE consumer reconciles from the
// store after a wake and on a timer, so correctness never depends on this
// process-local hub or on retaining a lossy event log.
type Hub struct {
	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{}
}

func NewHub() *Hub { return &Hub{subs: map[string]map[chan struct{}]struct{}{}} }

func (h *Hub) Publish(conversationID string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	for ch := range h.subs[conversationID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	h.mu.Unlock()
}

func (h *Hub) Subscribe(conversationID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.subs[conversationID] == nil {
		h.subs[conversationID] = map[chan struct{}]struct{}{}
	}
	h.subs[conversationID][ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs[conversationID], ch)
			if len(h.subs[conversationID]) == 0 {
				delete(h.subs, conversationID)
			}
			h.mu.Unlock()
		})
	}
	return ch, cancel
}
