package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/chat"
)

const (
	chatSSEDefaultReconcile = time.Second
	chatSSEHeartbeat        = 15 * time.Second
	chatSSEWriteTimeout     = 15 * time.Second
)

var errChatStreamStop = errors.New("chat stream: stop without an error event")

// chatStreamTiming is an optional test/embedding hook. Production Services
// use the bounded defaults above without needing another cmd setting.
type chatStreamTiming interface {
	chatStreamIntervals() (reconcile, heartbeat, writeTimeout time.Duration)
}

func chatSSEIntervals(svc ChatService) (time.Duration, time.Duration, time.Duration) {
	reconcile, heartbeat, writeTimeout := chatSSEDefaultReconcile, chatSSEHeartbeat, chatSSEWriteTimeout
	if timed, ok := svc.(chatStreamTiming); ok {
		if d, h, w := timed.chatStreamIntervals(); d > 0 && h > 0 && w > 0 {
			reconcile, heartbeat, writeTimeout = d, h, w
		}
	}
	return reconcile, heartbeat, writeTimeout
}

type chatSSEWriter struct {
	s          *Server
	svc        ChatService
	caller     *Caller
	course     string
	agent      string
	controller *http.ResponseController
	writeEvery time.Duration
}

func (w *chatSSEWriter) assertionLive() bool {
	return w.caller.ExpiresAt.IsZero() || w.s.o.Now().Before(w.caller.ExpiresAt)
}

func (w *chatSSEWriter) writeFrame(rw http.ResponseWriter, event string, payload any) error {
	var frame []byte
	if payload == nil {
		frame = []byte(event)
	} else {
		data, err := json.Marshal(payload)
		if err != nil {
			return errors.New("chat stream: cannot encode safe event")
		}
		// Append without precomputing a caller-influenced capacity: an
		// overflowing size expression must never become an allocation size.
		frame = append(frame, "event: "...)
		frame = append(frame, event...)
		frame = append(frame, "\ndata: "...)
		frame = append(frame, data...)
		frame = append(frame, '\n', '\n')
	}
	// A live peer gets a fresh write deadline for every frame and heartbeat;
	// an abandoned peer cannot hold a handler indefinitely.
	if err := w.controller.SetWriteDeadline(time.Now().Add(w.writeEvery)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	if _, err := rw.Write(frame); err != nil {
		return err
	}
	return w.controller.Flush()
}

func (w *chatSSEWriter) emit(rw http.ResponseWriter, r *http.Request, event string, payload any) error {
	if !w.assertionLive() {
		return errChatStreamStop
	}
	_, err := w.svc.CheckAccess(r.Context(), w.caller.ActorID, w.course, w.agent)
	if err != nil {
		if chatAPIError(err).Code == CodeNotFound {
			return errChatStreamStop
		}
		return err
	}
	return w.writeFrame(rw, event, payload)
}

func (w *chatSSEWriter) emitError(rw http.ResponseWriter, err error) {
	e := chatAPIError(err)
	var envelope errorBody
	envelope.Error.Code, envelope.Error.Message = e.Code, e.Message
	envelope.Error.Details = map[string]any{"reason": e.Reason}
	for k, v := range e.Details {
		envelope.Error.Details[k] = v
	}
	_ = w.writeFrame(rw, "error", envelope)
}

func sameChatDraft(a, b *chat.Draft) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Version == b.Version && reflect.DeepEqual(a, b)
}

// reconcileChatSSE derives every event from two store snapshots. No event
// log, process-local generation state, or hub payload is involved.
func reconcileChatSSE(last, next chat.Snapshot) (retracted []chat.Message, draft, message, state bool) {
	previous := make(map[string]chat.Message, len(last.Messages))
	for _, m := range last.Messages {
		previous[m.ID] = m
	}
	for _, m := range next.Messages {
		old, existed := previous[m.ID]
		if m.Status == "retracted" && (!existed || old.Status != "retracted") {
			retracted = append(retracted, m)
			continue
		}
		if !existed || !reflect.DeepEqual(old, m) {
			message = true
		}
	}
	// Approval/review transitions are store-backed snapshot changes too. A
	// review can finish without changing conversation.State or publication;
	// clients still need the new submission/review_state.
	message = message || !reflect.DeepEqual(last.Submission, next.Submission)
	draft = !sameChatDraft(last.Draft, next.Draft)
	state = last.Conversation.State != next.Conversation.State || last.Conversation.CloseReason != next.Conversation.CloseReason
	return retracted, draft, message, state
}

func (s *Server) chatConversationStream(w http.ResponseWriter, r *http.Request, c *Caller) {
	svc := s.chatService(w)
	if svc == nil {
		return
	}
	course, conversationID := r.PathValue("course_id"), r.PathValue("conversation_id")
	conversation, err := svc.Get(r.Context(), c.ActorID, course, conversationID)
	if err != nil {
		writeChatError(w, err)
		return
	}
	if !c.ExpiresAt.IsZero() && !s.o.Now().Before(c.ExpiresAt) {
		WriteError(w, Error{Code: CodeUnauthenticated, Reason: "expired", Message: "the assertion has expired: reconnect with a fresh one"})
		return
	}
	if _, err := svc.CheckAccess(r.Context(), c.ActorID, course, conversation.AgentID); err != nil {
		writeChatError(w, err)
		return
	}

	// Subscribe before the first snapshot read. A local write racing that read
	// therefore leaves a wake behind; the periodic store read covers all other
	// processes and a wake lost for any other reason.
	var wakes <-chan struct{}
	unsubscribe := func() {}
	if hub := svc.Hub(); hub != nil {
		wakes, unsubscribe = hub.Subscribe(conversationID)
	}
	defer unsubscribe()
	last, err := svc.Messages(r.Context(), c.ActorID, course, conversationID, chatDefaultPageSize, 0)
	if err != nil {
		writeChatError(w, err)
		return
	}
	last = normalizeChatSnapshot(last)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	if err := controller.SetReadDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		writeChatError(w, err)
		return
	}
	reconcileEvery, heartbeatEvery, writeEvery := chatSSEIntervals(svc)
	writer := &chatSSEWriter{s: s, svc: svc, caller: c, course: course, agent: conversation.AgentID,
		controller: controller, writeEvery: writeEvery}
	w.WriteHeader(http.StatusOK)
	if err := writer.emit(w, r, "snapshot", last); err != nil {
		if !errors.Is(err, errChatStreamStop) {
			writer.emitError(w, err)
		}
		return
	}
	if err := writer.writeFrame(w, ": connected\n\n", nil); err != nil {
		return
	}
	if last.Conversation.State == chat.Closed {
		return
	}

	reconcile := time.NewTicker(reconcileEvery)
	defer reconcile.Stop()
	heartbeat := time.NewTicker(heartbeatEvery)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if !writer.assertionLive() {
				return
			}
			if _, err := svc.CheckAccess(r.Context(), c.ActorID, course, conversation.AgentID); err != nil {
				if chatAPIError(err).Code != CodeNotFound {
					writer.emitError(w, err)
				}
				return
			}
			if err := writer.writeFrame(w, ": ping\n\n", nil); err != nil {
				return
			}
		case <-wakes:
			if err := s.reconcileChatStream(w, r, writer, &last); err != nil {
				if !errors.Is(err, errChatStreamStop) {
					writer.emitError(w, err)
				}
				return
			}
			if last.Conversation.State == chat.Closed {
				return
			}
		case <-reconcile.C:
			if err := s.reconcileChatStream(w, r, writer, &last); err != nil {
				if !errors.Is(err, errChatStreamStop) {
					writer.emitError(w, err)
				}
				return
			}
			if last.Conversation.State == chat.Closed {
				return
			}
		}
	}
}

func (s *Server) reconcileChatStream(rw http.ResponseWriter, r *http.Request, writer *chatSSEWriter, last *chat.Snapshot) error {
	if !writer.assertionLive() {
		return errChatStreamStop
	}
	if _, err := writer.svc.CheckAccess(r.Context(), writer.caller.ActorID, writer.course, writer.agent); err != nil {
		if chatAPIError(err).Code == CodeNotFound {
			return errChatStreamStop
		}
		return err
	}
	next, err := writer.svc.Messages(r.Context(), writer.caller.ActorID, writer.course, r.PathValue("conversation_id"), chatDefaultPageSize, 0)
	if err != nil {
		return err
	}
	next = normalizeChatSnapshot(next)
	// Store revisions move forwards. A delayed read must not make the browser
	// apply an older conversation or draft after a newer one.
	if next.Conversation.Revision < last.Conversation.Revision {
		return nil
	}
	retracted, draftChanged, messagesChanged, stateChanged := reconcileChatSSE(*last, next)
	for _, m := range retracted {
		payload := struct {
			Conversation chat.Conversation `json:"conversation"`
			Retracted    chat.Message      `json:"retracted"`
			Draft        *chat.Draft       `json:"draft"`
		}{next.Conversation, m, next.Draft}
		if err := writer.emit(rw, r, "retracted", payload); err != nil {
			return err
		}
	}
	if draftChanged {
		payload := struct {
			ConversationID string      `json:"conversation_id"`
			Revision       int64       `json:"revision"`
			Draft          *chat.Draft `json:"draft"`
		}{next.Conversation.ID, next.Conversation.Revision, next.Draft}
		if err := writer.emit(rw, r, "draft", payload); err != nil {
			return err
		}
	}
	if messagesChanged {
		if err := writer.emit(rw, r, "message", next); err != nil {
			return err
		}
	}
	if stateChanged {
		payload := struct {
			Conversation chat.Conversation `json:"conversation"`
		}{next.Conversation}
		if err := writer.emit(rw, r, "state", payload); err != nil {
			return err
		}
	}
	*last = next
	return nil
}
