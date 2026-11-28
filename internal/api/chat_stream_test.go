package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/chat"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

type chatSSEFrame struct {
	event, data, comment string
}

func readChatSSEFrame(r *bufio.Reader) (chatSSEFrame, error) {
	var frame chatSSEFrame
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return chatSSEFrame{}, err
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" {
			if frame.event != "" || frame.comment != "" {
				return frame, nil
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, ":"):
			frame.comment = strings.TrimSpace(strings.TrimPrefix(line, ":"))
		case strings.HasPrefix(line, "event: "):
			frame.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if frame.data != "" {
				frame.data += "\n"
			}
			frame.data += strings.TrimPrefix(line, "data: ")
		}
	}
}

func readChatSSEEvent(r *bufio.Reader) (chatSSEFrame, error) {
	for {
		frame, err := readChatSSEFrame(r)
		if err != nil {
			return chatSSEFrame{}, err
		}
		if frame.event != "" {
			return frame, nil
		}
	}
}

func openChatSSE(t *testing.T, f *fixture, token string) (*http.Response, *bufio.Reader, context.CancelFunc) {
	t.Helper()
	server := httptest.NewServer(f.s.Handler())
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+chatCoursePath("conversations/conv-1/stream"), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := server.Client().Do(request)
	if err != nil {
		cancel()
		server.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = response.Body.Close()
	})
	return response, bufio.NewReader(response.Body), cancel
}

func requireChatSSESnapshot(t *testing.T, response *http.Response, reader *bufio.Reader) chat.Snapshot {
	t.Helper()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("stream status %d: %s", response.StatusCode, body)
	}
	if got := response.Header.Get("Content-Type"); got != "text/event-stream; charset=utf-8" {
		t.Errorf("Content-Type %q", got)
	}
	if got := response.Header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control %q", got)
	}
	if got := response.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering %q", got)
	}
	frame, err := readChatSSEEvent(reader)
	if err != nil || frame.event != "snapshot" {
		t.Fatalf("initial frame %+v: %v", frame, err)
	}
	var snap chat.Snapshot
	if err := json.Unmarshal([]byte(frame.data), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Conversation.ID != "conv-1" || len(snap.Messages) == 0 {
		t.Fatalf("not a complete snapshot: %s", frame.data)
	}
	connected, err := readChatSSEFrame(reader)
	if err != nil || connected.comment != "connected" {
		t.Fatalf("connected frame %+v: %v", connected, err)
	}
	return snap
}

func TestChatSSESnapshotDraftMessageRetractedStatePingAndClose(t *testing.T) {
	svc := newFakeChatService()
	svc.snap.Conversation.State = chat.Answering
	svc.reconcile, svc.heartbeat = 20*time.Millisecond, 40*time.Millisecond
	f, token := newChatFixture(t, svc)
	response, reader, _ := openChatSSE(t, f, token)
	defer func() { _ = response.Body.Close() }()
	requireChatSSESnapshot(t, response, reader)

	frame, err := readChatSSEFrame(reader)
	if err != nil || frame.comment != "ping" {
		t.Fatalf("heartbeat frame %+v: %v", frame, err)
	}
	if checks, _ := svc.counts(); checks < 3 { // connect, snapshot event, heartbeat
		t.Errorf("only %d access checks before heartbeat", checks)
	}

	// No Hub.Publish: this is another process's draft write. Polling PostgreSQL
	// must be sufficient, not merely an optimization for same-process writes.
	svc.mu.Lock()
	svc.snap.Conversation.Revision = 3
	svc.snap.Draft = &chat.Draft{Version: 1, Text: "draft text", Steps: []chat.Step{{Kind: "thinking", State: "running"}}, UpdatedAt: at}
	svc.mu.Unlock()
	frame, err = readChatSSEEvent(reader)
	if err != nil || frame.event != "draft" || !strings.Contains(frame.data, `"conversation_id":"conv-1"`) ||
		!strings.Contains(frame.data, `"revision":3`) || !strings.Contains(frame.data, `"text":"draft text"`) {
		t.Fatalf("cross-instance draft frame %+v: %v", frame, err)
	}

	svc.mu.Lock()
	svc.snap.Conversation.Revision = 4
	svc.snap.Conversation.State = chat.Answered
	svc.snap.Draft = nil
	svc.snap.Messages = append(svc.snap.Messages, chat.Message{ID: "msg-2", ConversationID: "conv-1", Seq: 2,
		Role: "assistant", Status: "posted", Body: "final answer", CreatedAt: at})
	svc.mu.Unlock()
	for _, want := range []string{"draft", "message", "state"} {
		frame, err = readChatSSEEvent(reader)
		if err != nil || frame.event != want {
			t.Fatalf("want %s frame, got %+v: %v", want, frame, err)
		}
		if want == "draft" && !strings.Contains(frame.data, `"draft":null`) {
			t.Errorf("draft clear: %s", frame.data)
		}
		if want == "message" && (!strings.Contains(frame.data, `"id":"msg-2"`) || !strings.Contains(frame.data, `"final answer"`)) {
			t.Errorf("final message snapshot: %s", frame.data)
		}
	}

	retractedAt := at.Add(time.Minute)
	svc.mu.Lock()
	svc.snap.Conversation.Revision = 5
	svc.snap.Messages[0].Status = "retracted"
	svc.snap.Messages[0].Body = ""
	svc.snap.Messages[0].RetractedAt = &retractedAt
	svc.snap.Messages[0].RetractedReason = "withdrawn"
	svc.mu.Unlock()
	frame, err = readChatSSEEvent(reader)
	if err != nil || frame.event != "retracted" || !strings.Contains(frame.data, `"retracted":{"id":"msg-1"`) ||
		!strings.Contains(frame.data, `"body":""`) || !strings.Contains(frame.data, `"draft":null`) {
		t.Fatalf("retracted frame %+v: %v", frame, err)
	}

	reviewed := "reviewed"
	svc.mu.Lock()
	svc.snap.Conversation.Revision = 6
	svc.snap.Submission = &store.ChatSubmission{Status: store.ChatSubmissionExecuted, ApprovalID: "approval-1", ReviewState: &reviewed}
	svc.mu.Unlock()
	frame, err = readChatSSEEvent(reader)
	if err != nil || frame.event != "message" || !strings.Contains(frame.data, `"review_state":"reviewed"`) {
		t.Fatalf("review-only snapshot frame %+v: %v", frame, err)
	}

	svc.mu.Lock()
	svc.snap.Conversation.Revision = 7
	svc.snap.Conversation.State = chat.Closed
	svc.snap.Conversation.CloseReason = "closed_by_opener"
	svc.mu.Unlock()
	frame, err = readChatSSEEvent(reader)
	if err != nil || frame.event != "state" || !strings.Contains(frame.data, `"state":"closed"`) {
		t.Fatalf("closed state frame %+v: %v", frame, err)
	}
	if _, err := readChatSSEFrame(reader); !errors.Is(err, io.EOF) {
		t.Errorf("closed conversation did not end stream: %v", err)
	}
}

func TestChatSSEReconnectAlwaysSendsSnapshot(t *testing.T) {
	svc := newFakeChatService()
	f, token := newChatFixture(t, svc)
	response, reader, cancel := openChatSSE(t, f, token)
	requireChatSSESnapshot(t, response, reader)
	cancel()
	_ = response.Body.Close()

	svc.mu.Lock()
	svc.snap.Conversation.Revision = 9
	svc.snap.Draft = &chat.Draft{Version: 4, Text: "persisted elsewhere", Steps: []chat.Step{}, UpdatedAt: at}
	svc.mu.Unlock()
	response2, reader2, _ := openChatSSE(t, f, token)
	defer func() { _ = response2.Body.Close() }()
	snap := requireChatSSESnapshot(t, response2, reader2)
	if snap.Conversation.Revision != 9 || snap.Draft == nil || snap.Draft.Version != 4 {
		t.Errorf("reconnect depended on an event log instead of current state: %+v", snap)
	}
}

func TestChatSSEAssertionExpiryAndAccessRevocationEndStream(t *testing.T) {
	t.Run("assertion expiry", func(t *testing.T) {
		svc := newFakeChatService()
		svc.reconcile = 15 * time.Millisecond
		f, token := newChatFixture(t, svc)
		response, reader, _ := openChatSSE(t, f, token)
		defer func() { _ = response.Body.Close() }()
		requireChatSSESnapshot(t, response, reader)
		f.add(5 * time.Minute)
		if _, err := readChatSSEFrame(reader); !errors.Is(err, io.EOF) {
			t.Errorf("expired assertion did not end stream: %v", err)
		}
	})
	t.Run("access revoked", func(t *testing.T) {
		svc := newFakeChatService()
		svc.reconcile = 15 * time.Millisecond
		f, token := newChatFixture(t, svc)
		response, reader, _ := openChatSSE(t, f, token)
		defer func() { _ = response.Body.Close() }()
		requireChatSSESnapshot(t, response, reader)
		svc.setCheckErr(chat.ErrDenied)
		if _, err := readChatSSEFrame(reader); !errors.Is(err, io.EOF) {
			t.Errorf("revoked access did not end stream: %v", err)
		}
	})
	t.Run("safe authorization failure", func(t *testing.T) {
		svc := newFakeChatService()
		svc.reconcile = 15 * time.Millisecond
		f, token := newChatFixture(t, svc)
		response, reader, _ := openChatSSE(t, f, token)
		defer func() { _ = response.Body.Close() }()
		requireChatSSESnapshot(t, response, reader)
		svc.setCheckErr(errors.New("Core/provider: bearer secret and upstream private text"))
		frame, err := readChatSSEEvent(reader)
		if err != nil || frame.event != "error" || !strings.Contains(frame.data, `"code":"unavailable"`) {
			t.Fatalf("safe error frame %+v: %v", frame, err)
		}
		if strings.Contains(frame.data, "bearer secret") || strings.Contains(frame.data, "private text") {
			t.Errorf("secret/provider error leaked: %s", frame.data)
		}
	})
}

func TestChatSSESubscribesBeforeFirstSnapshotRead(t *testing.T) {
	svc := newFakeChatService()
	svc.onRead = func(call int) {
		if call == 1 {
			svc.hub.Publish("conv-1")
		}
	}
	f, token := newChatFixture(t, svc)
	response, reader, _ := openChatSSE(t, f, token)
	defer func() { _ = response.Body.Close() }()
	requireChatSSESnapshot(t, response, reader)
	deadline := time.Now().Add(250 * time.Millisecond)
	for {
		_, reads := svc.counts()
		if reads >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the publish during the first snapshot read was lost: subscription happened too late")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

type chatDeadlineWriter struct {
	mu            sync.Mutex
	header        http.Header
	body          bytes.Buffer
	reads, writes []time.Time
	flushed       chan struct{}
}

func newChatDeadlineWriter() *chatDeadlineWriter {
	return &chatDeadlineWriter{header: http.Header{}, flushed: make(chan struct{}, 16)}
}

func (w *chatDeadlineWriter) Header() http.Header { return w.header }
func (w *chatDeadlineWriter) WriteHeader(int)     {}
func (w *chatDeadlineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.Write(p)
}
func (w *chatDeadlineWriter) Flush() {
	select {
	case w.flushed <- struct{}{}:
	default:
	}
}
func (w *chatDeadlineWriter) SetReadDeadline(at time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reads = append(w.reads, at)
	return nil
}
func (w *chatDeadlineWriter) SetWriteDeadline(at time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, at)
	return nil
}

func TestChatSSEClearsReadAndRefreshesWriteDeadlines(t *testing.T) {
	svc := newFakeChatService()
	svc.heartbeat = 10 * time.Millisecond
	f, token := newChatFixture(t, svc)
	writer := newChatDeadlineWriter()
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, chatCoursePath("conversations/conv-1/stream"), nil).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+token)
	done := make(chan struct{})
	go func() {
		f.s.Handler().ServeHTTP(writer, request)
		close(done)
	}()
	for range 3 { // snapshot, connected, heartbeat
		select {
		case <-writer.flushed:
		case <-time.After(time.Second):
			t.Fatal("stream did not flush every frame")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("request context cancellation did not end the stream")
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if len(writer.reads) == 0 || !writer.reads[0].IsZero() {
		t.Errorf("read deadline was not cleared: %v", writer.reads)
	}
	if len(writer.writes) < 3 {
		t.Fatalf("write deadline refreshed %d times, want at least 3", len(writer.writes))
	}
	for i := 1; i < len(writer.writes); i++ {
		if writer.writes[i].Before(writer.writes[i-1]) {
			t.Errorf("write deadlines moved backwards: %v", writer.writes)
		}
	}
	if !strings.Contains(writer.body.String(), ": ping\n\n") {
		t.Errorf("heartbeat was not written and flushed: %q", writer.body.String())
	}
}
