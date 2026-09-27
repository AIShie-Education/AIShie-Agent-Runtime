package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// person is someone in a world: an actor, their own token, and their seat
// in the course.
type person struct {
	name, id, token, member string
}

// agentSeat is an agent seated in a world's course: its actor, its token
// and the environment variable the runtime's env:// reference reads it
// from, and its seat.
type agentSeat struct {
	id, token, tokenVar, member string
}

// world is one test's own piece of Core, built as Core's scripts/e2e.sh
// builds its worked example: an admin, a term, a department and CS101 (A),
// activated; Sato, who teaches it and owns its tutor; Mori, a second
// instructor, who decides the tutor's proposals, since nobody decides their
// own agent's; the students Yuki and Ken; HW3, published; Yuki's own agent,
// seated as a student's is, by her request and Sato's approval; and Sato's
// course tutor. No two worlds share a course, a person or an agent, so the
// tests run side by side without seeing each other.
type world struct {
	api   *coreAPI
	root  string
	admin person
	// course is the course's id, and hw3 the assignment's.
	course, hw3           string
	sato, mori, yuki, ken person
	own, tutor            agentSeat
	// modelKey is the provider key the runtime's model is given, from the
	// environment variable modelKeyVar: it must reach no log either.
	modelKey, modelKeyVar string

	mu   sync.Mutex
	logs []capture
}

// capture is one log the world's runtime or binary wrote.
type capture struct {
	name string
	text func() string
}

// newWorld builds a world in Core for the test named slug, and puts its
// agents' tokens and its model key in the environment (t.Setenv), where
// the runtime's env:// references find them.
func newWorld(t *testing.T, api *coreAPI, root, slug string) *world {
	t.Helper()
	w := &world{api: api, root: root}
	env := strings.ToUpper(strings.ReplaceAll(slug, "-", "_"))

	adminID := result[struct {
		ActorID string `json:"actor_id"`
	}](t, api, root, "POST", "/v1/actors", map[string]any{"kind": "human", "display_name": "Admin", "platform_role": "admin"}).ActorID
	w.admin = person{name: "Admin", id: adminID, token: w.issueToken(t, root, "/v1/actors/"+adminID+"/tokens")}

	today := time.Now().UTC()
	term := result[struct {
		ID string `json:"id"`
	}](t, api, w.admin.token, "POST", "/v1/terms", map[string]any{"name": "E2E " + slug + " " + api.run,
		"starts_on": today.AddDate(0, 0, -30).Format(time.DateOnly), "ends_on": today.AddDate(0, 0, 120).Format(time.DateOnly)}).ID
	dept := result[struct {
		ID string `json:"id"`
	}](t, api, w.admin.token, "POST", "/v1/departments", map[string]any{"name": "Computing " + slug + " " + api.run}).ID
	course := result[struct {
		CourseID string `json:"course_id"`
		Root     string `json:"root_component_id"`
	}](t, api, w.admin.token, "POST", "/v1/courses", map[string]any{"dept_id": dept, "term_id": term, "code": "CS101", "section": "A",
		"title": "Introduction to Computing"})
	w.course = course.CourseID
	api.call(t, http.StatusOK, w.admin.token, "POST", w.path("/activate"), nil)

	w.sato, w.mori, w.yuki, w.ken = w.register(t, "Sato"), w.register(t, "Mori"), w.register(t, "Yuki"), w.register(t, "Ken")
	for _, p := range []*person{&w.sato, &w.mori} {
		p.member = w.memberID(t, w.admin.token, w.path("/instructors"), map[string]any{"actor_id": p.id})
	}
	bucket := result[struct {
		ID string `json:"id"`
	}](t, api, w.sato.token, "POST", w.path("/components"), map[string]any{"parent_id": course.Root, "name": "Assignments", "weight": 40}).ID
	w.hw3 = result[struct {
		ID string `json:"id"`
	}](t, api, w.sato.token, "POST", w.path("/assignments"), map[string]any{"title": "HW3", "points_possible": 100, "component_id": bucket}).ID
	api.call(t, http.StatusOK, w.sato.token, "POST", w.path("/assignments/"+w.hw3+"/publish"), nil)
	for _, p := range []*person{&w.yuki, &w.ken} {
		p.member = w.memberID(t, w.sato.token, w.path("/members"), map[string]any{"actor_id": p.id, "preset": "student"})
	}

	// Yuki's own agent: a student's request is a proposal (her
	// agent_delegate is confirm_required), which an instructor approves.
	w.own = w.newAgent(t, w.yuki, "Yuki's helper", "E2E_"+env+"_OWN_TOKEN")
	req := api.call(t, http.StatusAccepted, w.yuki.token, "POST", w.path("/delegates"), map[string]any{"actor_id": w.own.id, "preset": "delegate"})
	if req.Status != "proposed" || req.ActionID == "" {
		t.Fatalf("Yuki's request to seat her agent: %s; want proposed", req)
	}
	approved := result[struct {
		Outcome string `json:"outcome"`
		Result  struct {
			MemberID string `json:"member_id"`
		} `json:"result"`
	}](t, api, w.sato.token, "POST", w.path("/actions/"+req.ActionID+"/decide"), map[string]any{"decision": "approve"})
	if approved.Outcome != "executed" || approved.Result.MemberID == "" {
		t.Fatalf("Sato approved Yuki's agent, and it came to %+v", approved)
	}
	w.own.member = approved.Result.MemberID

	// Sato's tutor answers the course: seated at once, by the one who
	// manages its members.
	w.tutor = w.newAgent(t, w.sato, "CS101 Tutor", "E2E_"+env+"_TUTOR_TOKEN")
	w.tutor.member = w.memberID(t, w.sato.token, w.path("/delegates"), map[string]any{"actor_id": w.tutor.id, "preset": "course_tutor"})

	w.modelKeyVar = "E2E_" + env + "_MODEL_KEY"
	w.modelKey = "sk-e2e-" + randomHex(16)
	t.Setenv(w.own.tokenVar, w.own.token)
	t.Setenv(w.tutor.tokenVar, w.tutor.token)
	t.Setenv(w.modelKeyVar, w.modelKey)
	return w
}

// issueToken is a new token from path (an actor's or an agent's tokens),
// issued as token.
func (w *world) issueToken(t testing.TB, token, path string) string {
	t.Helper()
	tok := result[struct {
		Token string `json:"token"`
	}](t, w.api, token, "POST", path, map[string]any{"label": "e2e"}).Token
	if !strings.HasPrefix(tok, "ais_") {
		t.Fatalf("POST %s issued no token", path)
	}
	return tok
}

// register is the admin registering a person, with a token of their own.
func (w *world) register(t testing.TB, name string) person {
	t.Helper()
	id := result[struct {
		ActorID string `json:"actor_id"`
	}](t, w.api, w.admin.token, "POST", "/v1/actors", map[string]any{"kind": "human", "display_name": name}).ActorID
	return person{name: name, id: id, token: w.issueToken(t, w.admin.token, "/v1/actors/"+id+"/tokens")}
}

// newAgent is owner making an agent of their own (My agents) and a token
// for the runtime, kept in the environment variable tokenVar.
func (w *world) newAgent(t testing.TB, owner person, name, tokenVar string) agentSeat {
	t.Helper()
	id := result[struct {
		ActorID string `json:"actor_id"`
	}](t, w.api, owner.token, "POST", "/v1/me/agents", map[string]any{"display_name": name}).ActorID
	return agentSeat{id: id, token: w.issueToken(t, owner.token, "/v1/me/agents/"+id+"/tokens"), tokenVar: tokenVar}
}

// memberID is the seat a POST that seats someone made.
func (w *world) memberID(t testing.TB, token, path string, body any) string {
	t.Helper()
	m := result[struct {
		MemberID string `json:"member_id"`
	}](t, w.api, token, "POST", path, body).MemberID
	if m == "" {
		t.Fatalf("POST %s seated nobody: its result names no member", path)
	}
	return m
}

// path is a path under the world's course.
func (w *world) path(rest string) string { return "/v1/courses/" + w.course + rest }

// secret is a token or a key the world holds, for the check that no log
// holds one; what says whose it is, and never what it is.
type secret struct{ what, value string }

// secrets are every token and key of the world.
func (w *world) secrets() []secret {
	return []secret{
		{"root's token", w.root}, {"the admin's token", w.admin.token},
		{"Sato's token", w.sato.token}, {"Mori's token", w.mori.token}, {"Yuki's token", w.yuki.token}, {"Ken's token", w.ken.token},
		{"the token of Yuki's agent", w.own.token}, {"the tutor's token", w.tutor.token}, {"the model key", w.modelKey},
	}
}

// addLog records a log of the world's, for the check that no log holds a
// token or a key.
func (w *world) addLog(name string, text func() string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.logs = append(w.logs, capture{name: name, text: text})
}

// captures are the world's logs.
func (w *world) captures() []capture {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]capture(nil), w.logs...)
}

// message is a conversation's message as Core shows it.
type message struct {
	ID             string  `json:"id"`
	Seq            int64   `json:"seq"`
	AuthorMemberID string  `json:"author_member_id"`
	InReplyTo      *string `json:"in_reply_to_message_id"`
	Body           *string `json:"body"`
}

// text is the message's body, "" once retracted.
func (m message) text() string {
	if m.Body == nil {
		return ""
	}
	return *m.Body
}

// replyTo is the message this one answers, "" for a question.
func (m message) replyTo() string {
	if m.InReplyTo == nil {
		return ""
	}
	return *m.InReplyTo
}

// conversation is a conversation's view as Core shows it.
type conversation struct {
	ID                    string  `json:"id"`
	Status                string  `json:"status"`
	State                 string  `json:"state"`
	PendingReplyActionID  *string `json:"pending_reply_action_id"`
	LatestOpenerMessageID *string `json:"latest_opener_message_id"`
}

// pending is the answer waiting for approval, "" when none is.
func (c conversation) pending() string {
	if c.PendingReplyActionID == nil {
		return ""
	}
	return *c.PendingReplyActionID
}

// ask has p open a conversation with the member respondent, asking body,
// and returns the conversation and the question.
func (w *world) ask(t testing.TB, p person, respondent, body string) (conv, msg string) {
	t.Helper()
	r := result[struct {
		ConversationID string `json:"conversation_id"`
		MessageID      string `json:"message_id"`
	}](t, w.api, p.token, "POST", w.path("/conversations"), map[string]any{"respondent_member_id": respondent, "body": body})
	if r.ConversationID == "" || r.MessageID == "" {
		t.Fatalf("%s asked, and Core named no conversation or message", p.name)
	}
	return r.ConversationID, r.MessageID
}

// followUp has p write again in conv, and returns the message. It may be
// called from any goroutine: a model's responder writes follow-ups.
func (w *world) followUp(ctx context.Context, p person, conv, body string) (string, error) {
	r, err := w.api.send(ctx, p.token, "POST", w.path("/conversations/"+conv+"/ask"), map[string]any{"body": body}, "")
	if err != nil {
		return "", err
	}
	var res struct {
		MessageID string `json:"message_id"`
	}
	if r.HTTP != http.StatusOK || r.Status != "executed" || json.Unmarshal(r.Result, &res) != nil || res.MessageID == "" {
		return "", fmt.Errorf("%s's follow-up: %s", p.name, r)
	}
	return res.MessageID, nil
}

// messages are conv's newest messages as p reads them, oldest first.
func (w *world) messages(t testing.TB, p person, conv string) []message {
	t.Helper()
	return result[struct {
		Messages []message `json:"messages"`
	}](t, w.api, p.token, "GET", w.path("/conversations/"+conv+"/messages?limit=50"), nil).Messages
}

// answers are the messages in conv that the member author wrote, as p
// reads them.
func (w *world) answers(t testing.TB, p person, conv, author string) []message {
	t.Helper()
	var out []message
	for _, m := range w.messages(t, p, conv) {
		if m.AuthorMemberID == author {
			out = append(out, m)
		}
	}
	return out
}

// conversation is conv as p sees it.
func (w *world) conversation(t testing.TB, p person, conv string) conversation {
	t.Helper()
	return result[conversation](t, w.api, p.token, "GET", w.path("/conversations/"+conv), nil)
}

// setAnswerLevel is Sato setting his tutor's conversation_answer.
func (w *world) setAnswerLevel(t testing.TB, level string) {
	t.Helper()
	w.api.call(t, http.StatusOK, w.sato.token, "POST", w.path("/members/"+w.tutor.member+"/perms"),
		map[string]any{"perms": map[string]string{"conversation_answer": level}})
}

// decide is Mori deciding one of the tutor's proposals; reason is left out
// when empty. It returns the decision's outcome.
func (w *world) decide(t testing.TB, actionID, decision, reason string) string {
	t.Helper()
	body := map[string]any{"decision": decision}
	if reason != "" {
		body["reason"] = reason
	}
	return result[struct {
		Outcome string `json:"outcome"`
	}](t, w.api, w.mori.token, "POST", w.path("/actions/"+actionID+"/decide"), body).Outcome
}

// answerAs is the agent a calling conversation_answer itself, over REST,
// under key: a replay, when the runtime sent the same under that key.
func (w *world) answerAs(t testing.TB, a agentSeat, conv, inReplyTo, body, key string) reply {
	t.Helper()
	r, err := w.api.send(context.Background(), a.token, "POST", w.path("/conversations/"+conv+"/answer"),
		map[string]any{"in_reply_to_message_id": inReplyTo, "body": body}, key)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// action is one of an actor's actions as action_list_mine shows it.
type action struct {
	ID         string          `json:"id"`
	ActionType string          `json:"action_type"`
	Status     string          `json:"status"`
	Payload    json.RawMessage `json:"payload"`
	Result     json.RawMessage `json:"result"`
}

// actionsMine are the agent a's actions in the course, oldest first, as it
// reads them itself (GET …/actions/mine).
func (w *world) actionsMine(t testing.TB, a agentSeat) []action {
	t.Helper()
	q := url.Values{"limit": {"200"}}
	return result[struct {
		Actions []action `json:"actions"`
	}](t, w.api, a.token, "GET", w.path("/actions/mine?"+q.Encode()), nil).Actions
}
