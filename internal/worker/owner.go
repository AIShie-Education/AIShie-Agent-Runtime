package worker

import (
	"context"
	"errors"
	"reflect"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// OwnerProblem is why a hosted agent does not run as its owner's: the state
// it is stopped in, why as the API names it, and what that state says. It
// stays stopped until its configuration changes (its row in the
// registry), or a reload.
type OwnerProblem struct {
	// State is store.AgentOwnerChanged.
	State string
	// Reason is store.ReasonOwnerChanged.
	Reason string
	Detail string
}

func (p *OwnerProblem) Error() string { return p.Detail }

// markOwnerVerified records in the registry that Core named the hosted
// agent cfg's owner as its row does (agent_runtime.agent), when the row
// does not say so yet: a row stored before hosting by id, while holding a
// pasted token was the proof. The store's only way
// to write it is UpdateHostedAgent, whose If-Match (the version read here)
// keeps it from overwriting a change made meanwhile, and which moves the
// row's version and the registry's revision on; a row changed since the
// check (another owner, or written meanwhile) is left as it is, for its
// next start to check. Nothing here stops the agent: the agent runs
// whether or not the mark is written, and the rebuild the write sets off
// restarts it not (sameRun).
func (s *Supervisor) markOwnerVerified(ctx context.Context, cfg *config.Agent) {
	h := cfg.Hosted
	if h == nil || h.OwnerVerified {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer cancel()
	row, err := s.o.Store.HostedAgent(ctx, cfg.ID)
	if err != nil {
		s.log.Warn("the hosted agent's owner was not recorded as checked", "agent", cfg.ID, "err", err)
		return
	}
	if row.OwnerVerified || row.CoreActorID != h.CoreActorID || row.OwnerActorID != h.OwnerActorID {
		return
	}
	row.OwnerVerified = true
	switch _, err := s.o.Store.UpdateHostedAgent(ctx, *row); {
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrNotFound):
	case err != nil:
		s.log.Warn("the hosted agent's owner was not recorded as checked", "agent", cfg.ID, "err", err)
	default:
		s.log.Info("the hosted agent's owner is the one Core names; recorded as checked", "agent", cfg.ID)
	}
}

// olderRow reports whether version, of a hosted agent's row as a rebuild
// of the registry read it, is older than the row run runs: one its worker
// wrote after that read, as it kept the token it was issued (adopt). A
// row's version moves on at every write, and the registry's rebuilds are
// put in force in the order they read it, so only the worker's own write
// puts a runner ahead of them. 0, no hosted agent's, is older than none.
func olderRow(version int, run *config.Agent) bool {
	return version > 0 && run.Hosted != nil && version < run.Hosted.Version
}

// sameRun reports whether two configurations of one agent run it alike:
// equal, but for whether its owner has been checked, which the registry
// records of a hosted agent the check at its start passed
// (markOwnerVerified), and for the version of its row, which every write
// moves on: neither changes anything in how it runs, so that a write that
// changes nothing else restarts nothing (apply writes its state again,
// for the new version).
func sameRun(x, y *config.Agent) bool {
	if x.Hosted == nil || y.Hosted == nil {
		return reflect.DeepEqual(x, y)
	}
	xh, yh := *x.Hosted, *y.Hosted
	xh.OwnerVerified, xh.Version = yh.OwnerVerified, yh.Version
	xc, yc := *x, *y
	xc.Hosted, yc.Hosted = &xh, &yh
	return reflect.DeepEqual(&xc, &yc)
}
