package e2e

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/fakellm"
)

// longPollPickup is an idle agent noticing a question at once where Core's
// inbox waits for news (wait_s, Core 2c1fe1b; Core's docs/agent-runtime.md
// §7.2, whose target is p95 ≤ 1 s): the course tutor, at the default
// long_poll_wait_s of 25 s and with a schedule that would take 10 s or more,
// has a call waiting when Yuki asks, and claims each of her questions
// within a second of its being written. The pickup is measured two ways:
// the runtime's own notice latency (answer_latency_seconds{stage=notice}:
// the question written, by Core's clock, to its claim) and, from here, the
// question sent to the model being asked about it, which takes in Core's
// answer to the ask and the runtime reading the conversation.
func longPollPickup(t *testing.T, w *world) {
	const questions = 5
	var mu sync.Mutex
	var asked []time.Time
	m := newModel(t, func(fakellm.ChatRequest) fakellm.ChatResponse {
		mu.Lock()
		asked = append(asked, time.Now())
		mu.Unlock()
		return fakellm.Reply("On Friday.")
	})
	slow := map[string]any{"polling": map[string]any{"inbox_hot_s": 10, "hot_window_s": 10, "inbox_idle_s": 10, "inbox_max_s": 30,
		"long_poll_wait_s": 25}}
	rt := w.startRuntime(t, m, runtimeConf{agents: []agentConf{{id: "tutor", seat: w.tutor, over: slow}}})
	eventually(t, answerWait, "the tutor's inbox waiting for a question", func() bool { return rt.longPolls("tutor") >= 1 })

	var notices, pickups []time.Duration
	for i := range questions {
		// Idle: the call waiting has waited a while.
		time.Sleep(1500 * time.Millisecond)
		before := rt.histogramSum("answer_latency_seconds", map[string]string{"stage": "notice"})
		sent := time.Now()
		conv, _ := w.ask(t, w.yuki, w.tutor.member, fmt.Sprintf("Question %d: when is the lab report due?", i+1))
		w.waitAnswer(t, w.yuki, conv, w.tutor.member)
		eventually(t, answerWait, "the answer's notice latency recorded", func() bool {
			return rt.metric("answer_latency_seconds", map[string]string{"stage": "notice"}) >= float64(i+1)
		})
		notice := rt.histogramSum("answer_latency_seconds", map[string]string{"stage": "notice"}) - before
		notices = append(notices, time.Duration(notice*float64(time.Second)))
		mu.Lock()
		pickups = append(pickups, asked[len(asked)-1].Sub(sent))
		mu.Unlock()
		eventually(t, answerWait, "the tutor's inbox waiting again", func() bool { return rt.longPolls("tutor") >= 1 })
	}
	t.Logf("pickup of %d questions by an idle tutor long-polling its inbox: notice (written → claimed) %v, max %v; "+
		"asked → the model asked %v, max %v", questions, round(notices), round([]time.Duration{slices.Max(notices)})[0],
		round(pickups), round([]time.Duration{slices.Max(pickups)})[0])
	for i, n := range notices {
		if n > time.Second {
			t.Errorf("question %d was claimed %s after it was written; want within a second", i+1, n)
		}
	}
	if n := rt.metric("long_poll_fallbacks_total", nil); n != 0 {
		t.Errorf("%v long polls went back to the schedule", n)
	}
}

func round(ds []time.Duration) []time.Duration {
	out := make([]time.Duration, len(ds))
	for i, d := range ds {
		out[i] = d.Round(time.Millisecond)
	}
	return out
}
