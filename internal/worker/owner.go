package worker

import (
	"context"
	"errors"
	"reflect"
	"strings"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// What a hosted agent's state says when the check of its owner fails. They
// name no one: the state is shown to whoever the registry says owns the
// agent, who may no longer be its owner, and the ids of the people on
// either side are not theirs to read.
const (
	ownerChangedDetail = "the agent's owner in Core is no longer the person who connected it here: " +
		"its owner must connect it again"
	ownerGoneDetail = "Core names no owner for the agent now, so its owner there is no longer the person who connected it here: " +
		"once Core names one, its owner must connect it again"
	ownerUnknownDetail = "this Core does not say who owns an agent (its me_get has no owner_actor_id, as before Core's C1), " +
		"so the agent's owner cannot be checked and it does not run: Core must be upgraded, and the runtime reloaded"
)

// OwnerProblem is why a hosted agent does not run as its owner's: the state
// it is stopped in, why as the API names it, and what that state says. It
// stays stopped until its configuration changes (its row in the
// registry), or a reload.
type OwnerProblem struct {
	// State is store.AgentOwnerChanged, or store.AgentError for a Core
	// that cannot say who owns an agent.
	State string
	// Reason is store.ReasonOwnerChanged, or store.ReasonCoreTooOld.
	Reason string
	Detail string
}

func (p *OwnerProblem) Error() string { return p.Detail }

// HostedOwnerProblem checks the owner of the hosted agent cfg at its start:
// me is what me_get said with its token, and cat is its Core's catalogue.
// It is nil when Core names the owner the agent's row does, or cfg is not
// hosted (a YAML agent's owner is whoever its operator says).
//
// The agent does not run, in state owner_changed, when Core names another
// owner (the agent was given to someone else, or was connected by someone
// who held its token without owning it), or names none (its owner was taken
// away): a hosted agent answers only for the person who connected it here.
//
// me_get says nothing of an owner on a Core from before Core's C1, as it
// says nothing for an agent nobody owns, so its answer alone cannot tell
// the two apart; the catalogue can, since only a Core that says it
// describes owner_actor_id. On an older Core the owner cannot be verified,
// and holding the token is not taken as proof of it (the product owner's
// D5 allows that only while connecting, as a stopgap on edge): the
// agent does not run, in state error, which says that Core must be
// upgraded. Running it would put a model, and its owner's key, at the
// service of whoever holds the token now, which is what the check is for.
// Its owner connecting it again would not help, which is why the state is
// not owner_changed.
func HostedOwnerProblem(cfg *config.Agent, me *core.Actor, cat *core.Catalogue) *OwnerProblem {
	h := cfg.Hosted
	switch {
	case h == nil:
		return nil
	case me.OwnerActorID != "" && strings.EqualFold(me.OwnerActorID, h.OwnerActorID):
		// Actor ids are UUIDs, which Core writes in lower case; the
		// runtime compares them in any.
		return nil
	case me.OwnerActorID != "":
		return &OwnerProblem{State: store.AgentOwnerChanged, Reason: store.ReasonOwnerChanged, Detail: ownerChangedDetail}
	case !cat.MeGetNamesOwners():
		return &OwnerProblem{State: store.AgentError, Reason: store.ReasonCoreTooOld, Detail: ownerUnknownDetail}
	}
	return &OwnerProblem{State: store.AgentOwnerChanged, Reason: store.ReasonOwnerChanged, Detail: ownerGoneDetail}
}

// markOwnerVerified records in the registry that Core named the hosted
// agent cfg's owner as its row does, when the row does not say so yet: a
// row stored while holding the token was the proof. The store's only way
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
