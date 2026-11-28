package chat

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

const (
	DefaultDraftEvery = 300 * time.Millisecond
	maxDraftSteps     = 20
	maxDraftRunes     = 20000
	maxStepTarget     = 120
)

// DraftWriter coalesces Runner deltas and writes the newest complete draft at
// most once every 300 ms. Drafts are best effort; generation never depends on
// a successful draft write.
type DraftWriter struct {
	generation string
	holder     string
	fence      int64
	store      store.ChatStore
	hub        *Hub
	every      time.Duration
	hideText   bool
	wake       chan struct{}
	done       chan struct{}

	mu      sync.Mutex
	text    string
	steps   []store.ChatStep
	seq     uint64
	stopped bool
}

func NewDraftWriter(st store.ChatStore, hub *Hub, generation, holder string, fence int64, every time.Duration, hideText bool) *DraftWriter {
	if every <= 0 {
		every = DefaultDraftEvery
	}
	d := &DraftWriter{generation: generation, holder: holder, fence: fence, store: st, hub: hub, every: every, hideText: hideText, wake: make(chan struct{}, 1), done: make(chan struct{})}
	go d.run()
	return d
}

func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for at := range s {
		if i == n {
			return s[:at]
		}
		i++
	}
	return s
}

func safeTarget(s string) string {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	return cutRunes(s, maxStepTarget)
}

func (d *DraftWriter) Set(text string, steps []store.ChatStep) {
	if d.hideText {
		text = ""
	}
	text = cutRunes(strings.ToValidUTF8(text, "\uFFFD"), maxDraftRunes)
	if len(steps) > maxDraftSteps {
		steps = steps[len(steps)-maxDraftSteps:]
	}
	cloned := make([]store.ChatStep, len(steps))
	for i, step := range steps {
		step.Target = safeTarget(step.Target)
		if step.State != store.ChatPending && step.State != "done" {
			step.State = "running"
		}
		if step.State == store.ChatPending {
			step.State = "running"
		}
		cloned[i] = step
	}
	d.mu.Lock()
	if !d.stopped {
		d.text, d.steps, d.seq = text, cloned, d.seq+1
	}
	d.mu.Unlock()
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

func (d *DraftWriter) run() {
	defer close(d.done)
	var last time.Time
	var persisted uint64
	for {
		<-d.wake
		for {
			d.mu.Lock()
			if d.stopped {
				d.mu.Unlock()
				return
			}
			text, steps, seq := d.text, append([]store.ChatStep(nil), d.steps...), d.seq
			d.mu.Unlock()
			if seq == 0 || seq == persisted {
				break
			}
			if wait := time.Until(last.Add(d.every)); wait > 0 {
				t := time.NewTimer(wait)
				select {
				case <-t.C:
				case <-d.wake:
					t.Stop()
					continue
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			conv, _, written, err := d.store.PutChatDraft(ctx, d.generation, d.holder, d.fence, text, steps)
			cancel()
			last = time.Now()
			if err == nil && written {
				d.hub.Publish(conv.ID)
			}
			if err == nil {
				persisted = seq
			}
			d.mu.Lock()
			current, stopped := d.seq, d.stopped
			d.mu.Unlock()
			if stopped {
				return
			}
			if current == seq {
				break
			}
		}
	}
}

// Stop prevents any later draft write and waits for an in-flight store call.
// Final answer, failure and withdrawal clear the persisted draft atomically.
func (d *DraftWriter) Stop() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.stopped = true
	d.mu.Unlock()
	select {
	case d.wake <- struct{}{}:
	default:
	}
	<-d.done
}
