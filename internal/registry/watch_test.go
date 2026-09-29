package registry

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// rebuilds counts a watcher's rebuilds, each reading the store's revision
// as Build does.
type rebuilds struct {
	st interface {
		RegistryRev(context.Context) (int64, error)
	}
	n    atomic.Int32
	mu   sync.Mutex
	last int64
	fail atomic.Bool
}

func (r *rebuilds) rebuild(ctx context.Context) (int64, error) {
	if r.fail.Load() {
		return 0, errors.New("the database is down")
	}
	rev, err := r.st.RegistryRev(ctx)
	if err != nil {
		return 0, err
	}
	r.mu.Lock()
	r.last = rev
	r.mu.Unlock()
	r.n.Add(1)
	return rev, nil
}

func (r *rebuilds) rev() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// eventually waits for cond, failing the test after a deadline.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%v waiting for %s", errTimeout, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// run runs w until the test ends, and waits for it to return.
func run(t *testing.T, w *Watcher, from int64) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx, from) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the watcher did not return")
		}
	})
}

// createAgent hosts one agent, as the API would.
func createAgent(t *testing.T, st store.Store, id string) {
	t.Helper()
	a := row(id, ownModel)
	_, err := st.CreateHostedAgent(t.Context(), a, fakeSecret(a.TokenSecretID, a.TenantID, store.SecretCoreToken),
		fakeSecret(a.KeySecretID, a.TenantID, store.SecretModelKey))
	if err != nil {
		t.Fatal(err)
	}
}

// A write to the registry is put in force at once, by its notification:
// the poll here never comes. A write made by hand, not through the store,
// notifies as well.
func TestWatcherNotifyPath(t *testing.T) {
	u := pgDatabase(t)
	st := pgStore(t, u)
	r := &rebuilds{st: st}
	run(t, &Watcher{Rev: st.RegistryRev, Listen: st.ListenRegistry, Rebuild: r.rebuild, Poll: time.Hour}, 0)
	// Listening, it reads the registry once, for what changed before.
	eventually(t, "the first read, once it listens", func() bool { return r.n.Load() == 1 })

	createAgent(t, st, "agt_1")
	eventually(t, "a rebuild after the agent was created", func() bool { return r.n.Load() == 2 })
	if want, _ := st.RegistryRev(t.Context()); r.rev() != want {
		t.Errorf("rebuilt at %d, want %d", r.rev(), want)
	}
	if err := st.PutHostedCourse(t.Context(), store.HostedCourse{AgentID: "agt_1", CourseID: course1, Settings: json.RawMessage(`{"enabled": false}`)}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a rebuild after a course was put", func() bool { return r.n.Load() == 3 })
	execOn(t, u, `UPDATE hosted_agent SET paused = true WHERE id = 'agt_1'`)
	eventually(t, "a rebuild after a write made by hand", func() bool { return r.n.Load() == 4 })
}

// With no listener, the poll finds the revision moved on, and rebuilds;
// a revision that has not moved rebuilds nothing, and one that cannot be
// read is read again at the next poll.
func TestWatcherPollPath(t *testing.T) {
	u := pgDatabase(t)
	st := pgStore(t, u)
	r := &rebuilds{st: st}
	from, err := st.RegistryRev(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	run(t, &Watcher{Rev: st.RegistryRev, Rebuild: r.rebuild, Poll: 20 * time.Millisecond}, from)
	time.Sleep(100 * time.Millisecond)
	if n := r.n.Load(); n != 0 {
		t.Fatalf("%d rebuilds of a registry that did not change", n)
	}
	createAgent(t, st, "agt_1")
	eventually(t, "a rebuild after the agent was created", func() bool { return r.n.Load() == 1 })

	// A rebuild that fails is tried again at the next poll.
	r.fail.Store(true)
	if _, err := st.SetHostedAgentPaused(t.Context(), "agt_1", true, 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	r.fail.Store(false)
	eventually(t, "a rebuild once the store answers", func() bool { return r.n.Load() == 2 })
	if want, _ := st.RegistryRev(t.Context()); r.rev() != want {
		t.Errorf("rebuilt at %d, want %d", r.rev(), want)
	}
}

// A listener whose connection is lost listens again, reads the registry
// for what changed while it was not listening, and goes on putting writes
// in force at once.
func TestWatcherListensAgain(t *testing.T) {
	u := pgDatabase(t)
	st := pgStore(t, u)
	r := &rebuilds{st: st}
	var listens atomic.Int32
	listen := func(ctx context.Context, ready, changed func()) error {
		listens.Add(1)
		return st.ListenRegistry(ctx, ready, changed)
	}
	run(t, &Watcher{Rev: st.RegistryRev, Listen: listen, Rebuild: r.rebuild, Poll: time.Hour, Retry: 10 * time.Millisecond}, 0)
	eventually(t, "the first read, once it listens", func() bool { return r.n.Load() == 1 })

	execOn(t, u, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE datname = current_database() AND query LIKE 'LISTEN%' AND pid <> pg_backend_pid()`)
	// Written while the listener is down: read once it listens again.
	createAgent(t, st, "agt_1")
	eventually(t, "listening again", func() bool { return listens.Load() >= 2 })
	eventually(t, "the registry read again", func() bool {
		want, _ := st.RegistryRev(t.Context())
		return r.rev() == want
	})
	n := r.n.Load()
	createAgent(t, st, "agt_2")
	eventually(t, "a rebuild after a write, listening again", func() bool { return r.n.Load() > n })
}

// A database that does not answer holds the watcher up no longer than its
// timeout: neither a read of the revision nor a rebuild that hangs (a lock
// held, a connection lost without a word) keeps the poll from trying
// again.
func TestWatcherIsNotHeldUp(t *testing.T) {
	var cur atomic.Int64
	var revCalls, rebuildCalls, rebuilt atomic.Int32
	hang := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	rev := func(ctx context.Context) (int64, error) {
		if revCalls.Add(1) == 1 {
			return 0, hang(ctx)
		}
		return cur.Load(), nil
	}
	rebuild := func(ctx context.Context) (int64, error) {
		if rebuildCalls.Add(1) == 1 {
			return 0, hang(ctx)
		}
		rebuilt.Add(1)
		return cur.Load(), nil
	}
	cur.Store(1)
	run(t, &Watcher{Rev: rev, Rebuild: rebuild, Poll: 10 * time.Millisecond, Timeout: 50 * time.Millisecond}, 0)
	eventually(t, "a rebuild after a read and a rebuild that hung", func() bool { return rebuilt.Load() == 1 })
	if n := rebuildCalls.Load(); n != 2 {
		t.Errorf("%d rebuilds, want the one that hung and the one after", n)
	}
}
