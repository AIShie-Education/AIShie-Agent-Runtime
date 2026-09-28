package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store/memstore"
)

// gatedStates is a store whose writes of one state wait at a gate: the
// store is slow to take them, as a busy database is.
type gatedStates struct {
	*memstore.Store
	state   string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gatedStates) SetAgentState(ctx context.Context, st store.AgentState) error {
	if st.State == g.state {
		g.once.Do(func() { close(g.entered) })
		<-g.release
	}
	return g.Store.SetAgentState(ctx, st)
}

// TestARewriteNeverOutlivesTheStateAfterIt: when a hosted agent's row moves
// on while it starts, the supervisor writes its state again for the new
// version; if the agent comes to run while the store is still taking that
// rewrite, the store ends with running, not with the starting it read
// first. Each agent's state writes reach the store in the order they are
// made.
func TestARewriteNeverOutlivesTheStateAfterIt(t *testing.T) {
	ctx := context.Background()
	st := &gatedStates{Store: memstore.New(), state: store.AgentStarting, entered: make(chan struct{}), release: make(chan struct{})}
	sup, err := NewSupervisor(Options{Config: &config.Config{}, Store: st, WorkerID: "w"})
	if err != nil {
		t.Fatal(err)
	}
	r := &runner{id: "a", cfg: &config.Agent{ID: "a", Hosted: &config.Hosted{Version: 2}}, holds: true, state: store.AgentStarting}
	sup.mu.Lock()
	sup.runners["a"] = r
	sup.mu.Unlock()

	rewritten := make(chan struct{})
	go func() {
		defer close(rewritten)
		sup.rewriteState(ctx, r)
	}()
	<-st.entered // the rewrite read starting, and the store is taking it
	ran := make(chan struct{})
	go func() {
		defer close(ran)
		sup.writeState(ctx, "a", store.AgentRunning, "", "")
	}()
	select {
	case <-ran:
	case <-time.After(200 * time.Millisecond):
	}
	close(st.release)
	<-rewritten
	<-ran

	got, err := st.AgentState(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.AgentRunning || got.ConfigVersion != 2 {
		t.Errorf("the store after a rewrite and the agent's running: %+v, want running at version 2", got)
	}
	sup.mu.Lock()
	defer sup.mu.Unlock()
	if r.state != store.AgentRunning {
		t.Errorf("the runner's state: %s, want running", r.state)
	}
}
