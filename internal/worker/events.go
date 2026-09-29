package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

const (
	// eventsPage is how many events one read of event_list asks for, Core's
	// most: a seat's first read pages through the course's history.
	eventsPage = 500
	// maxEventPages bounds the pages one round reads; the rest wait for the
	// next round, from the cursor saved.
	maxEventPages = 50
	// actionsPage is how many actions one read of action_list_mine asks
	// for, Core's most.
	actionsPage = 200
	// maxActionPages bounds the pages one lookup of the agent's actions
	// reads.
	maxActionPages = 50
)

// Action statuses as action_list_mine reports them.
const (
	actionProposed  = "proposed"
	actionExecuted  = "executed"
	actionFailed    = "failed"
	actionRejected  = "rejected"
	actionCancelled = "cancelled"
)

// pollEvents follows the seat's events (design §5.4) until ctx is done:
// every events_s, and at once and 5, 15 and 45 s after an answer is
// proposed. First it sends again what a crash left sending, and settles
// what was decided while the runtime was down.
func (s *Seat) pollEvents(ctx context.Context) {
	s.recover(ctx)
	for ctx.Err() == nil {
		s.readEvents(ctx)
		r := s.a.rand()
		for {
			d := s.nextEvents(r).Sub(s.a.now())
			if d <= 0 {
				break
			}
			t := time.NewTimer(d)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			case <-s.wakeEvents:
				t.Stop()
			}
		}
	}
}

// nextEvents is when events are read next: events_s after the last read
// (jittered, and doubled while the agent is slowed), or the next follow-up
// of a proposal, whichever comes first.
func (s *Seat) nextEvents(r float64) time.Time {
	p := s.polling()
	d := config.Seconds(p.EventsS)
	if s.a.slow() {
		d *= 2
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.lastEvents.Add(Jitter(d, p.Jitter, r))
	for len(s.followUps) > 0 && !s.followUps[0].After(s.lastEvents) {
		s.followUps = s.followUps[1:]
	}
	if len(s.followUps) > 0 && s.followUps[0].Before(next) {
		next = s.followUps[0]
	}
	return next
}

// readEvents reads event_list from the seat's cursor until caught up,
// acting on each event, and saves the cursor after each page. Core's
// next_seq may stay where it was when the page held nothing the agent may
// see: that is caught up, not an error.
func (s *Seat) readEvents(ctx context.Context) {
	ctx = core.WithPriority(ctx, core.PriorityBackground)
	st := s.a.store()
	raw, err := st.Cursor(ctx, s.a.id, s.id, store.CursorEvents)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("the events cursor could not be read", "err", err)
		}
		s.markEventsRead()
		return
	}
	since, _ := strconv.ParseInt(raw, 10, 64)
	acts := &actionLookup{s: s}
	defer acts.save(ctx)
	for page := 0; page < maxEventPages; page++ {
		evs, err := s.a.client.Events(ctx, s.course, since, eventsPage, 0)
		s.markEventsRead()
		if err != nil {
			s.readFailed(ctx, "event_list", err)
			return
		}
		for _, ev := range evs.Events {
			if err := s.onEvent(ctx, ev, acts); err != nil {
				// The cursor stays before this page: its events are read,
				// and acted on, again next time. Acting on one twice does
				// no harm.
				if ctx.Err() == nil {
					s.log.Warn("an event could not be acted on; it is read again next time", "type", ev.Type, "seq", ev.Seq, "err", err)
				}
				return
			}
		}
		if evs.NextSeq <= since {
			return
		}
		since = evs.NextSeq
		if err := st.SetCursor(ctx, s.a.id, s.id, store.CursorEvents, strconv.FormatInt(since, 10)); err != nil && ctx.Err() == nil {
			s.log.Warn("the events cursor could not be saved", "err", err)
		}
		if !evs.More {
			return
		}
	}
}

func (s *Seat) markEventsRead() {
	now := s.a.now()
	s.mu.Lock()
	s.lastEvents = now
	s.mu.Unlock()
}

// onEvent acts on one event. Only what concerns an attempt the store
// holds, the agent's own conversations, or its answers is acted on, so
// that a seat reading its whole history on its first read does no harm.
// Its error is the store failing, when the event must be read again.
func (s *Seat) onEvent(ctx context.Context, ev core.Event, acts *actionLookup) error {
	switch ev.Type {
	case core.EventActionApproved, core.EventActionRejected, core.EventActionCancelled:
		if ev.ActionID == nil {
			return nil
		}
		at, err := s.a.store().AttemptByAction(ctx, s.a.id, *ev.ActionID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return nil
		case err != nil:
			return err
		case at.State != store.AttemptProposed:
			return nil
		}
		if act, ok := acts.find(ctx, *ev.ActionID); ok && act.Status != actionProposed {
			return s.settleProposal(at, act)
		}
		return s.settleProposal(at, actionFromEvent(ev))
	case core.EventConversationMessageRetracted:
		var p struct {
			ConversationID string `json:"conversation_id"`
			MessageID      string `json:"message_id"`
		}
		if json.Unmarshal(ev.Payload, &p) == nil && p.ConversationID != "" && p.MessageID != "" {
			return s.retracted(ctx, p.ConversationID, p.MessageID)
		}
	case core.EventConversationMessagePosted:
		var p struct {
			AuthorMemberID string `json:"author_member_id"`
			OpenerMemberID string `json:"opener_member_id"`
		}
		if json.Unmarshal(ev.Payload, &p) != nil || p.AuthorMemberID != p.OpenerMemberID || p.AuthorMemberID == s.id {
			return nil
		}
		// The opener writing makes the course hot, if it is news.
		if at, err := time.Parse(time.RFC3339Nano, ev.OccurredAt); err == nil &&
			s.a.now().Sub(at) < config.Seconds(s.polling().HotWindowS) {
			s.markHot()
		}
	}
	return nil
}

// actionFromEvent is what an event says of a proposal's fate, for when
// action_list_mine does not show it.
func actionFromEvent(ev core.Event) core.Action {
	var p struct {
		Outcome string `json:"outcome"`
		Error   string `json:"error"`
		Reason  string `json:"reason"`
	}
	_ = json.Unmarshal(ev.Payload, &p)
	act := core.Action{ID: *ev.ActionID}
	switch ev.Type {
	case core.EventActionApproved:
		act.Status = p.Outcome
		if p.Outcome == actionFailed && p.Error != "" {
			act.Result, _ = json.Marshal(map[string]any{"error": map[string]string{"code": p.Error}})
		}
	case core.EventActionRejected:
		act.Status = actionRejected
	case core.EventActionCancelled:
		act.Status = actionCancelled
		act.Result, _ = json.Marshal(map[string]any{"error": map[string]any{"details": map[string]string{"reason": p.Reason}}})
	}
	return act
}

// settleProposal settles a proposed attempt as its action stands (§2.4):
// executed, it posted (the course is hot, and memory notes it); rejected,
// the reason goes into the conversation's memory for the next attempt;
// cancelled (expired, most often), memory notes why; failed, nothing was
// posted. Whatever did not post puts the conversation back in the inbox,
// for the next attempt. Its error is the store failing to record it.
func (s *Seat) settleProposal(at *store.Attempt, act core.Action) error {
	o := store.Outcome{ActionID: act.ID}
	var note *store.Note
	base := store.Note{AgentID: s.a.id, MemberID: s.id, ConversationID: at.ConversationID, MessageID: at.MessageID}
	switch act.Status {
	case actionExecuted:
		o.State = store.AttemptExecuted
		o.PostedMessageID = actionMessageID(act)
		if at.Tool == toolAnswer {
			n := base
			n.Kind, n.Text, n.MessageID = store.NoteAnswered, answeredNote(o.PostedMessageID), o.PostedMessageID
			note = &n
		}
	case actionRejected:
		o.State, o.Reason = store.AttemptRejected, act.DecisionReason()
		n := base
		n.Kind, n.Text = store.NoteRejected, o.Reason
		note = &n
	case actionCancelled:
		o.State, o.Reason = store.AttemptCancelled, actionErrorReason(act)
		n := base
		n.Kind, n.Text = store.NoteCancelled, o.Reason
		note = &n
	case actionFailed:
		o.State, o.ErrorCode = store.AttemptFailed, actionErrorCode(act)
	default:
		return nil
	}
	ctx, cancel := bookkeeping()
	defer cancel()
	if err := s.a.store().FinishAttempt(ctx, s.a.id, at.Key, o); err != nil {
		s.log.Error("proposal not settled", "key", at.Key, "err", err)
		return err
	}
	if note != nil {
		addNote(s.a, s.config(), *note)
	}
	s.log.Info("a proposal was decided", "conversation", at.ConversationID, "key", at.Key, "action", act.ID, "status", act.Status)
	if o.State == store.AttemptExecuted {
		s.markHot()
	} else {
		poke(s.wakeInbox)
	}
	return nil
}

// actionMessageID is the message an executed answer's action made.
func actionMessageID(act core.Action) string {
	var r struct {
		MessageID string `json:"message_id"`
	}
	if len(act.Result) == 0 || json.Unmarshal(act.Result, &r) != nil {
		return ""
	}
	return r.MessageID
}

// actionError is a failed or cancelled action's stored error.
type actionError struct {
	Error struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

func actionErrorCode(act core.Action) string {
	var e actionError
	if len(act.Result) == 0 || json.Unmarshal(act.Result, &e) != nil {
		return ""
	}
	return e.Error.Code
}

// actionErrorReason is a cancelled action's reason (proposal_expired,
// member_removed, …).
func actionErrorReason(act core.Action) string {
	var e actionError
	if len(act.Result) == 0 || json.Unmarshal(act.Result, &e) != nil {
		return ""
	}
	r, _ := e.Error.Details["reason"].(string)
	return r
}

// retracted forgets what memory holds about a retracted message; when it
// was the agent's own answer, memory notes not to repeat it, and the owner
// is told (§6.3). A retraction read again, its note written already, is
// left as it is. Its error is the store failing.
func (s *Seat) retracted(ctx context.Context, conv, msg string) error {
	st := s.a.store()
	notes, err := st.Notes(ctx, s.a.id, s.id, conv, memoryNotes)
	if err != nil {
		return err
	}
	own := false
	for _, n := range notes {
		switch {
		case n.MessageID != msg:
		case n.Kind == store.NoteRetractedOwn:
			return nil
		case n.Kind == store.NoteAnswered:
			own = true
		}
	}
	if err := st.ForgetMessage(ctx, s.a.id, s.id, conv, msg); err != nil {
		return err
	}
	if !own {
		return nil
	}
	addNote(s.a, s.config(), store.Note{AgentID: s.a.id, MemberID: s.id, ConversationID: conv, Kind: store.NoteRetractedOwn,
		Text: "An answer of yours here was retracted.", MessageID: msg})
	s.log.Warn("an answer of the agent's was retracted", "conversation", conv, "message", msg)
	s.a.tell("an answer of the agent's was retracted (conversation " + conv + ", message " + msg + ")")
	return nil
}

// actionLookup finds the agent's own actions in the seat's course, paging
// action_list_mine (all types: the answers are what matter) from the
// seat's actions cursor only as far as it must. The cursor moves only over
// actions that are settled, so that a proposal of the runtime's own (an
// answer or a close) still waiting is found again when it is decided,
// however long that takes; save keeps it. A proposal of the model's own
// writes is its owner's to follow, not the runtime's, and holds the cursor
// back no more than a settled action does.
type actionLookup struct {
	s     *Seat
	begun bool
	// start is the cursor as read; after, where the next page starts;
	// keep, the last action of the settled run from start.
	start, after, keep string
	blocked            bool
	done               bool
	pages              int
	acts               map[string]core.Action
}

// find is the action id, reading pages until it is found or there are no
// more.
func (l *actionLookup) find(ctx context.Context, id string) (core.Action, bool) {
	if !l.begun {
		l.begun, l.acts = true, map[string]core.Action{}
		cur, err := l.s.a.store().Cursor(ctx, l.s.a.id, l.s.id, store.CursorActions)
		if err != nil {
			l.done = true
		}
		l.start, l.after, l.keep = cur, cur, cur
	}
	for {
		if act, ok := l.acts[id]; ok {
			return act, true
		}
		if l.done || !l.more(ctx) {
			return core.Action{}, false
		}
	}
}

// more reads the next page, and reports whether it read one.
func (l *actionLookup) more(ctx context.Context) bool {
	if l.pages >= maxActionPages {
		l.done = true
		return false
	}
	l.pages++
	page, err := l.s.a.client.ActionsMine(core.WithPriority(ctx, core.PriorityBackground), l.s.course, l.after, nil, actionsPage)
	if err != nil {
		l.s.readFailed(ctx, "action_list_mine", err)
		l.done = true
		return false
	}
	for _, act := range page.Actions {
		l.acts[act.ID] = act
		if act.Status == actionProposed && runtimesOwn(act.ActionType) {
			l.blocked = true
		}
		if !l.blocked {
			l.keep = act.ID
		}
	}
	if len(page.Actions) < actionsPage {
		l.done = true
	} else {
		l.after = page.Actions[len(page.Actions)-1].ID
	}
	return true
}

// runtimesOwn reports whether an action of this type is one the runtime
// makes itself, and follows: an answer, or a close.
func runtimesOwn(actionType string) bool {
	return actionType == "conversation.answer" || actionType == "conversation.close"
}

// save keeps the cursor, when it moved.
func (l *actionLookup) save(ctx context.Context) {
	if !l.begun || l.keep == l.start {
		return
	}
	if err := l.s.a.store().SetCursor(ctx, l.s.a.id, l.s.id, store.CursorActions, l.keep); err != nil && ctx.Err() == nil {
		l.s.log.Warn("the actions cursor could not be saved", "err", err)
	}
}

// recover is the seat's start (design §5.4): every attempt a crash or a
// timeout left sending is sent again with its stored bytes, and every
// proposal is settled as Core has it now.
func (s *Seat) recover(ctx context.Context) {
	atts, err := s.a.store().Unsettled(ctx, s.a.id, s.id)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("unsettled attempts not read", "err", err)
		}
		return
	}
	var proposed []store.Attempt
	for _, at := range atts {
		switch at.State {
		case store.AttemptSending:
			s.resendAtStart(ctx, at)
		case store.AttemptProposed:
			proposed = append(proposed, at)
		}
	}
	if len(proposed) == 0 || ctx.Err() != nil {
		return
	}
	acts := &actionLookup{s: s}
	defer acts.save(ctx)
	for _, at := range proposed {
		if act, ok := acts.find(ctx, at.ActionID); ok && act.Status != actionProposed {
			_ = s.settleProposal(&at, act) // logged; the event, or the next start, settles it
		}
	}
}

// resendAtStart sends one attempt left sending again, as a claim would,
// under the conversation's lease, and records its end in the ledger.
func (s *Seat) resendAtStart(ctx context.Context, at store.Attempt) {
	if !s.a.sched.reserve(at.ConversationID) {
		return
	}
	defer s.a.sched.release(at.ConversationID)
	c := &claim{s: s, a: s.a, eff: s.config(), conv: at.ConversationID, claimedAt: s.a.now()}
	if !c.lease(ctx) {
		return
	}
	defer c.release()
	ctx, cancel := context.WithTimeout(core.WithPriority(ctx, core.PriorityAnswer), c.eff.Budgets.PerAnswer.WallClock()+passSlack)
	defer cancel()
	s.log.Info("an attempt written ahead is sent again", "conversation", at.ConversationID, "key", at.Key)
	env, err := s.a.client.Send(ctx, at.Tool, at.Args)
	var d Decision
	if at.Tool == toolClose {
		d = classifyClose(env, err)
	} else {
		d = Classify(env, err)
	}
	settle(s.a, c.eff, at, env, d)
	switch d.Next {
	case NextDone:
		if at.Tool == toolAnswer {
			s.markHot()
			c.noteAnswered(at.MessageID, messageID(env))
		}
	case NextProposed:
		s.followUp()
	case NextHoldSeat:
		s.holdSeat("Core denied an answer sent again")
	case NextDropReseat:
		s.a.requestReseat()
	case NextStopAgent:
		s.a.stop(core.ErrUnauthenticated)
	}
	// The claim that wrote it ended without a ledger row that says what
	// became of it: this is that row. Who asked is not known here.
	r := passResult{msg: at.MessageID, no: at.No, key: at.Key, kind: at.Kind, outcome: d.Outcome, postAt: s.a.now(), postedID: messageID(env)}
	switch {
	case at.Tool == toolAnswer && d.Next == NextDone:
		r.posted, r.outcome = true, postedOutcome(at.Kind)
	case d.Next == NextProposed:
		r.posted = true
	}
	c.record(r)
}
