package fakecore

import (
	"bytes"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The test controls: the people and the world around the agents under test.
// Writes a person makes (asking, following up, retracting, closing,
// deciding, reviewing) go through the same pipeline and rules as a call
// would, so a test cannot script what Core would refuse; what an
// administrator does (seating, levels, pausing, archiving) is applied
// directly.

// Actor is a person or an agent in the fake.
type Actor struct {
	ID   string
	Name string
	// Kind is human or agent.
	Kind string
	// Token is an API token of the actor's, for MCP and REST.
	Token string
}

// Course is a course, with the IDs of its canned material: one assignment
// (HW1) with instructions, a syllabus of text, and lecture slides as a file.
type Course struct {
	ID, Code, Section, Title string

	AssignmentID   string
	InstructionsID string
	SyllabusID     string
	SlidesID       string
	// ComponentID is the grade component the assignment counts toward.
	ComponentID string
}

// Member is a seat: one actor in one course.
type Member struct {
	ID, CourseID, ActorID string
}

// SeatOptions say how an actor is seated.
type SeatOptions struct {
	// Preset is one of Core's built-in presets: student, observer, ta,
	// instructor, tutor, grader, delegate, course_tutor. Empty starts from
	// every permission denied.
	Preset string
	// Perms are levels by permission, over the preset's: denied,
	// confirm_required, pending_review or autonomous.
	Perms map[string]string
	// AnswersCourse overrides Core's default, which is true for a
	// course_tutor seated under a principal who manages the course's
	// members.
	AnswersCourse *bool
	// Principal is the owner's seat, for an agent a person owns: the agent
	// is seated as its delegate, and never holds more than it.
	Principal string
	// Role overrides the preset's roster role.
	Role string
	// StudentScope and AssignmentScope override the preset's kind of scope
	// (all or listed); ListedStudents and ListedAssignments name whom and
	// what a listed one reaches. By default, as Core seats: a student lists
	// themself; a delegate takes its principal's list, or nobody when the
	// principal reaches the whole class; anyone else listed reaches nobody.
	StudentScope, AssignmentScope     string
	ListedStudents, ListedAssignments []string
	// ExpiresAt ends the seat, as Core's expires_at does.
	ExpiresAt *time.Time
}

// Conversation is a conversation a control opened.
type Conversation struct {
	ID, CourseID string
}

// Message is a message a control wrote.
type Message struct {
	ID, ConversationID string
	Seq                int
}

// MessageRecord is a message as the fake holds it, for assertions.
type MessageRecord struct {
	ID             string
	ConversationID string
	Seq            int
	AuthorMemberID string
	// InReplyTo is the message an answer answers; "" for a question.
	InReplyTo string
	// Body is what was written, retracted or not.
	Body      string
	Retracted bool
	// ActionID and IdempotencyKey are those of the call that wrote it: for
	// an answer that was approved, the proposal's.
	ActionID       string
	IdempotencyKey string
	CreatedAt      time.Time
}

// Proposal is an action waiting for a person's decision.
type Proposal struct {
	ActionID       string
	ActionType     string
	MemberID       string
	IdempotencyKey string
	// Args are the call's arguments, without the key.
	Args json.RawMessage
}

// Work is a student's submission to a course's assignment, and its posted
// grade.
type Work struct {
	SubmissionID, GradeID string
}

// RefusedError is a control Core would have refused: what it answered.
type RefusedError struct {
	Tool, Status, Code, Reason, Message string
}

func (e *RefusedError) Error() string {
	s := fmt.Sprintf("fakecore: %s: %s", e.Tool, e.Status)
	if e.Code != "" {
		s += " " + e.Code
	}
	if e.Reason != "" {
		s += " (" + e.Reason + ")"
	}
	return s + ": " + e.Message
}

func refused(tool string, out outcome) error {
	e := &RefusedError{Tool: tool, Status: out.Status}
	if out.Error != nil {
		e.Code, e.Message = out.Error.Code, out.Error.Message
		e.Reason, _ = out.Error.Details["reason"].(string)
	}
	return e
}

// prefixEncoding is how Core writes a token's public prefix: base32, in
// lower case, of which 12 characters are kept.
var prefixEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// newToken is a token in Core's shape (Core's auth.NewToken), ais_, a
// public prefix of 12 characters of base32 in lower case, _, and 256 bits
// of secret in base64url; and its prefix, by which Core lists it.
func newToken() (token, prefix string) {
	var p [8]byte
	var secret [32]byte
	_, _ = rand.Read(p[:])
	_, _ = rand.Read(secret[:])
	prefix = strings.ToLower(prefixEncoding.EncodeToString(p[:]))[:12]
	return "ais_" + prefix + "_" + base64.RawURLEncoding.EncodeToString(secret[:]), prefix
}

// issue issues act a token, from issuer, labelled label, and returns the
// credential. Called with the lock held.
func (c *Core) issue(act, issuer *actor, label string) *credential {
	token, prefix := newToken()
	cr := &credential{id: newID(), token: token, prefix: prefix, actor: act, issuer: issuer, label: label, createdAt: c.now()}
	c.tokens[token] = cr
	return cr
}

// AddCourse makes an active course, section A, titled by its code, with
// its canned material.
func (c *Core) AddCourse(code string) Course {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	co := &course{id: newID(), code: code, section: "A", title: code, status: statusActive, deptID: newID(), termID: newID(), createdAt: now}
	root := &component{id: newID(), name: "Course total", weight: "100"}
	bucket := &component{id: newID(), name: "Assignments", weight: "100", parent: root}
	co.rootComponent, co.bucket, co.components = root, bucket, []*component{root, bucket}
	author := newID()
	doc := func(kind, title string, sort int) *document {
		d := &document{id: newID(), kind: kind, title: title, course: co, sortOrder: sort, createdAt: now, versionID: newID(),
			authorMemberID: author, versionCreatedAt: now}
		co.documents = append(co.documents, d)
		return d
	}
	syllabus := doc(kindMaterial, "Syllabus", 0)
	syllabus.bodyMD = ptr("# " + code + " syllabus\n\nWeekly lectures, one assignment a fortnight, and a final exam.")
	slides := doc(kindMaterial, "Lecture 1 slides", 1)
	slides.file, slides.contentType, slides.fileToken = []byte("%PDF-1.4\n% fakecore: lecture 1 slides\n"), ptr("application/pdf"), fileToken()
	c.blobs[slides.fileToken] = slides
	instructions := doc(kindInstructions, "HW1 instructions", 2)
	instructions.bodyMD = ptr("Answer the three questions at the end of chapter 1. Show your working.")
	published := now
	hw1 := &assignment{id: newID(), title: "HW1", component: bucket, instructions: instructions, points: "100", publishedAt: &published}
	co.assignments = []*assignment{hw1}
	c.courses[co.id] = co
	// What Core's feed says of a course made and opened by an administrator,
	// which every seat that reads documents sees.
	for _, typ := range []string{"course.created", "course.activated"} {
		by := newID()
		c.flush([]*event{{typ: typ, course: co, actionID: &by, subjectType: "course", subjectID: &co.id}})
	}
	return Course{ID: co.id, Code: co.code, Section: co.section, Title: co.title, AssignmentID: hw1.id,
		InstructionsID: instructions.id, SyllabusID: syllabus.id, SlidesID: slides.id, ComponentID: bucket.id}
}

// AddFile adds a material to the course, published, whose one version is
// a file: data, of contentType ("" for a file whose type was not
// recorded), as a person uploads a deck of slides or a handout.
// document_get gives it a download_url as Core does, and serves it. It
// returns the document's id.
func (c *Core) AddFile(courseID, title, contentType string, data []byte) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	co := c.courses[courseID]
	if co == nil {
		return "", fmt.Errorf("fakecore: AddFile: no course %s", courseID)
	}
	now := c.now()
	d := &document{id: newID(), kind: kindMaterial, title: title, course: co, sortOrder: len(co.documents), createdAt: now,
		versionID: newID(), authorMemberID: newID(), versionCreatedAt: now, file: bytes.Clone(data), fileToken: fileToken()}
	if contentType != "" {
		d.contentType = &contentType
	}
	co.documents = append(co.documents, d)
	c.blobs[d.fileToken] = d
	return d.id, nil
}

func ptr[T any](v T) *T { return &v }

// fileToken is the secret part of a file's download URL.
func fileToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// addActor registers an actor and issues it a token: a person's, labelled
// "record" and issued by itself; an agent's, labelled "runtime" and issued
// by its owner, as the recorder issues them through Core.
func (c *Core) addActor(name, kind string, owner *actor) Actor {
	a := &actor{id: newID(), kind: kind, name: name, status: statusActive, owner: owner}
	c.actors[a.id] = a
	issuer, label := a, "record"
	if owner != nil {
		issuer, label = owner, "runtime"
	}
	cr := c.issue(a, issuer, label)
	return Actor{ID: a.id, Name: name, Kind: kind, Token: cr.token}
}

// AddPerson registers a person and issues them a token.
func (c *Core) AddPerson(name string) Actor {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.addActor(name, "human", nil)
}

// AddAgent creates an agent the person ownerID owns, and issues it a token.
// It is seated only as its owner's delegate.
func (c *Core) AddAgent(name, ownerID string) (Actor, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	owner := c.actors[ownerID]
	if owner == nil || owner.kind != "human" {
		return Actor{}, fmt.Errorf("fakecore: AddAgent: %s is not a person here", ownerID)
	}
	return c.addActor(name, "agent", owner), nil
}

// AddUnownedAgent registers an agent nobody owns, as an administrator's
// actor.register does, and issues it a token. It is seated as anyone is
// (Seat without Principal), as member.add seats it.
func (c *Core) AddUnownedAgent(name string) Actor {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.addActor(name, "agent", nil)
}

// SetOwner gives the agent agentID the person ownerID as its owner, or
// takes its owner away when ownerID is "", as an administrator's
// actor.set_owner does in Core: refused while the agent is seated in a
// course that is not archived, and revoking every token the agent has,
// since whoever owned it before may hold them. IssueToken issues it the
// next.
func (c *Core) SetOwner(agentID, ownerID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.actors[agentID]
	if a == nil || a.kind != "agent" {
		return fmt.Errorf("fakecore: SetOwner: %s is not an agent here", agentID)
	}
	var owner *actor
	if ownerID != "" {
		if owner = c.actors[ownerID]; owner == nil || owner.kind != "human" {
			return fmt.Errorf("fakecore: SetOwner: %s is not a person here", ownerID)
		}
	}
	if a.owner == owner {
		return errors.New("fakecore: SetOwner: the agent already has that owner")
	}
	for _, m := range c.memberList {
		if m.actor == a && m.status != statusRemoved && m.course.status != statusArchived {
			return fmt.Errorf("fakecore: SetOwner: %s is seated in %s: take it out first", a.name, m.course.code)
		}
	}
	a.owner = owner
	now := c.now()
	for _, cr := range c.tokens {
		if cr.actor == a && !cr.revoked() {
			cr.revokedAt = &now
		}
	}
	return nil
}

// IssueToken issues the actor another token, labelled "runtime", from its
// owner (itself, for an actor nobody owns).
func (c *Core) IssueToken(actorID string) (string, error) {
	t, err := c.IssueLabelledToken(actorID, "runtime")
	return t.Token, err
}

// Token is a token the fake issued: the token, and its credential as
// credential_list names it.
type Token struct {
	Token        string
	CredentialID string
	// Prefix is the 12 characters after ais_ that Core lists it by.
	Prefix string
}

// IssueLabelledToken issues the actor another token labelled label, from
// its owner (itself, for an actor nobody owns), as agent.issue_token and
// credential.issue_token do.
func (c *Core) IssueLabelledToken(actorID, label string) (Token, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.actors[actorID]
	if a == nil {
		return Token{}, fmt.Errorf("fakecore: IssueToken: no actor %s", actorID)
	}
	issuer := a
	if a.owner != nil {
		issuer = a.owner
	}
	cr := c.issue(a, issuer, label)
	return Token{Token: cr.token, CredentialID: cr.id, Prefix: cr.prefix}, nil
}

// Revoke revokes a token: its next call is a 401.
func (c *Core) Revoke(token string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cr := c.tokens[token]
	if cr == nil {
		return errors.New("fakecore: Revoke: no such token")
	}
	if !cr.revoked() {
		now := c.now()
		cr.revokedAt = &now
	}
	return nil
}

// CredentialRecord is one of an actor's tokens as the fake holds it, for
// assertions.
type CredentialRecord struct {
	ID, Prefix, Label string
	CreatedAt         time.Time
	LastUsedAt        *time.Time
	RevokedAt         *time.Time
}

// Credentials lists the actor's tokens, newest first, as credential_list
// orders them, revoked ones included.
func (c *Core) Credentials(actorID string) []CredentialRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []CredentialRecord
	for _, cr := range c.credentialsOf(c.actors[actorID]) {
		r := CredentialRecord{ID: cr.id, Prefix: cr.prefix, Label: cr.label, CreatedAt: cr.createdAt}
		if cr.lastUsed != nil {
			t := *cr.lastUsed
			r.LastUsedAt = &t
		}
		if cr.revokedAt != nil {
			t := *cr.revokedAt
			r.RevokedAt = &t
		}
		out = append(out, r)
	}
	return out
}

// SuspendActor suspends the actor, as an administrator's actor.suspend or
// its owner's agent.suspend does: every call it makes is denied
// (actor_not_active), me_get and the credential tools among them, and its
// tokens are kept.
func (c *Core) SuspendActor(actorID string) error {
	return c.setActorStatus("SuspendActor", actorID, statusSuspended)
}

// ReactivateActor lifts a suspension: the actor's calls work again.
func (c *Core) ReactivateActor(actorID string) error {
	return c.setActorStatus("ReactivateActor", actorID, statusActive)
}

func (c *Core) setActorStatus(control, actorID, status string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.actors[actorID]
	if a == nil {
		return fmt.Errorf("fakecore: %s: no actor %s", control, actorID)
	}
	if a.status == status {
		return fmt.Errorf("fakecore: %s: the actor is %s already", control, status)
	}
	a.status = status
	return nil
}

// Seat seats an actor in a course, as member.add, member.add_delegate and
// course.seat_instructor seat people in Core.
func (c *Core) Seat(actorID, courseID string, o SeatOptions) (Member, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, co := c.actors[actorID], c.courses[courseID]
	switch {
	case a == nil:
		return Member{}, fmt.Errorf("fakecore: Seat: no actor %s", actorID)
	case co == nil:
		return Member{}, fmt.Errorf("fakecore: Seat: no course %s", courseID)
	case co.status == statusArchived:
		return Member{}, errors.New("fakecore: Seat: the course is archived")
	case c.seatOf(a, co) != nil:
		return Member{}, fmt.Errorf("fakecore: Seat: %s is seated in %s already", a.name, co.code)
	}
	var pr preset
	if o.Preset != "" {
		var ok bool
		if pr, ok = presets[o.Preset]; !ok {
			return Member{}, fmt.Errorf("fakecore: Seat: there is no preset %q", o.Preset)
		}
	} else {
		pr = preset{role: "student", studentScope: scopeListed, assignmentScope: scopeAll}
	}
	principal, err := c.principalFor(a, co, o.Principal)
	if err != nil {
		return Member{}, err
	}
	m := &member{id: newID(), course: co, actor: a, status: statusActive, expiresAt: o.ExpiresAt, role: pr.role,
		perms: map[string]level{}, principal: principal, createdAt: c.now()}
	if o.Preset != "" {
		m.presetID = c.presetID(o.Preset)
	}
	if o.Role != "" {
		m.role = o.Role
	}
	for i, p := range allPerms {
		m.perms[p] = pr.levels[i]
		if principal != nil {
			m.perms[p] = min(m.perms[p], delegateCap(principal, p))
		}
	}
	for p, name := range o.Perms {
		l, err := parseLevel(name)
		if err != nil || !validPerm(p) {
			return Member{}, fmt.Errorf("fakecore: Seat: %s: %s is not a permission and level", p, name)
		}
		if principal != nil && l > delegateCap(principal, p) {
			return Member{}, fmt.Errorf("fakecore: Seat: a delegate may not hold %s at %s: its principal holds less", p, name)
		}
		m.perms[p] = l
	}
	if err := c.scopeSeat(m, pr, o); err != nil {
		return Member{}, err
	}
	manages := principal != nil && principal.perm(permMemberManage).allowed()
	m.answersCourse = manages && o.Preset == "course_tutor"
	if o.AnswersCourse != nil {
		if *o.AnswersCourse && !manages {
			return Member{}, errors.New("fakecore: Seat: only a delegate of someone who manages the course's members answers the course")
		}
		m.answersCourse = *o.AnswersCourse
	}
	c.members[m.id] = m
	c.memberList = append(c.memberList, m)
	added := map[string]any{"actor_id": a.id, "role": m.role}
	if principal != nil {
		added["delegate"], added["principal_member_id"], added["answers_course"] = true, principal.id, m.answersCourse
	}
	c.flushStamped([]*event{memberEvent("member.added", m, added)})
	return Member{ID: m.id, CourseID: co.id, ActorID: a.id}, nil
}

// principalFor checks the principal an actor is seated under: an agent a
// person owns is seated as its owner's delegate, and nobody else is.
func (c *Core) principalFor(a *actor, co *course, principalID string) (*member, error) {
	if principalID == "" {
		if a.owner != nil {
			return nil, fmt.Errorf("fakecore: Seat: %s is %s's agent, and is seated only as their delegate: give Principal", a.name, a.owner.name)
		}
		return nil, nil
	}
	p := c.members[principalID]
	switch {
	case p == nil || p.course != co || p.status == statusRemoved:
		return nil, fmt.Errorf("fakecore: Seat: no seat %s in the course to be principal", principalID)
	case p.principal != nil:
		return nil, errors.New("fakecore: Seat: a delegate brings no agents of its own")
	case a.owner == nil || p.actor != a.owner:
		return nil, fmt.Errorf("fakecore: Seat: %s is not an agent of the principal's", a.name)
	}
	return p, nil
}

// scopeSeat gives a new seat its reach, as Core seats one.
func (c *Core) scopeSeat(m *member, pr preset, o SeatOptions) error {
	var err error
	own := func(ids []string) map[string]bool {
		out := map[string]bool{}
		for _, id := range ids {
			out[id] = true
		}
		return out
	}
	students, assignments := pr.studentScope, pr.assignmentScope
	if o.StudentScope != "" {
		students = o.StudentScope
	}
	if o.AssignmentScope != "" {
		assignments = o.AssignmentScope
	}
	if p := m.principal; p != nil {
		m.studentScope, m.students = delegateReach(p.studentScope, p.students, students, o.StudentScope != "", o.ListedStudents)
		m.assignmentScope, m.assignments = delegateReach(p.assignmentScope, p.assignments, assignments, o.AssignmentScope != "", o.ListedAssignments)
	} else {
		m.studentScope, m.assignmentScope = students, assignments
		m.students, m.assignments = own(o.ListedStudents), own(o.ListedAssignments)
		// A student listed with nobody lists itself, as Core seats one: it
		// sees its own work for the reason a tutor listed for it does.
		if o.ListedStudents == nil && m.role == "student" && m.studentScope == scopeListed {
			m.students = map[string]bool{m.id: true}
		}
	}
	for _, s := range []string{m.studentScope, m.assignmentScope} {
		if s != scopeAll && s != scopeListed {
			err = fmt.Errorf("fakecore: Seat: a scope is all or listed, not %q", s)
		}
	}
	return err
}

// delegateReach is a delegate's scope: the kind named, else the preset's,
// listed when its principal's is; the list named, else its principal's, or
// nobody when the principal reaches everyone.
func delegateReach(principalKind string, principalList map[string]bool, kind string, named bool, list []string) (string, map[string]bool) {
	if !named && principalKind == scopeListed {
		kind = scopeListed
	}
	out := map[string]bool{}
	switch {
	case list != nil:
		for _, id := range list {
			out[id] = true
		}
	case kind == scopeListed && principalKind == scopeListed:
		for id := range principalList {
			out[id] = true
		}
	}
	return kind, out
}

// flushStamped files events an administrator's action caused, as Core
// files them under that action; here it is an action of nobody the tests
// see, so it has an id and no row.
func (c *Core) flushStamped(evs []*event) {
	by := newID()
	for _, e := range evs {
		if e.actionID == nil {
			e.actionID = &by
		}
	}
	c.flush(evs)
}

func memberEvent(typ string, m *member, payload map[string]any) *event {
	id := m.id
	return &event{typ: typ, course: m.course, subjectType: "course_member", subjectID: &id, payload: mustJSON(payload)}
}

// member is the seat memberID, or an error naming the control.
func (c *Core) member(control, memberID string) (*member, error) {
	m := c.members[memberID]
	if m == nil {
		return nil, fmt.Errorf("fakecore: %s: no seat %s", control, memberID)
	}
	return m, nil
}

// actAs makes a call as the seat's actor, through the pipeline, and
// returns its result; anything but executed is a *RefusedError.
func (c *Core) actAs(m *member, name string, args map[string]any) (json.RawMessage, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	c.nextKey++
	key := "fakecore:" + name + ":" + strconv.Itoa(c.nextKey)
	out := c.invoke(m.actor, c.cat.byName[name], raw, key, c.opts.BaseURL)
	if out.Status != actExecuted {
		return nil, refused(mcpName(name), out)
	}
	return out.Result, nil
}

// Ask opens a conversation from the seat openerID to respondentID with its
// first question, as conversation_open does.
func (c *Core) Ask(courseID, openerID, respondentID, body string) (Conversation, Message, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, err := c.member("Ask", openerID)
	if err != nil {
		return Conversation{}, Message{}, err
	}
	res, err := c.actAs(m, "conversation.open", map[string]any{"course_id": courseID, "respondent_member_id": respondentID, "body": body})
	if err != nil {
		return Conversation{}, Message{}, err
	}
	var out struct {
		ConversationID string `json:"conversation_id"`
		MessageID      string `json:"message_id"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return Conversation{}, Message{}, err
	}
	return Conversation{ID: out.ConversationID, CourseID: courseID}, Message{ID: out.MessageID, ConversationID: out.ConversationID, Seq: 1}, nil
}

// FollowUp is the conversation's opener writing again, as
// conversation_ask does.
func (c *Core) FollowUp(conversationID, body string) (Message, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cv := c.conversations[conversationID]
	if cv == nil {
		return Message{}, fmt.Errorf("fakecore: FollowUp: no conversation %s", conversationID)
	}
	res, err := c.actAs(cv.opener, "conversation.ask", map[string]any{"course_id": cv.course.id, "conversation_id": cv.id, "body": body})
	if err != nil {
		return Message{}, err
	}
	var out struct {
		MessageID string `json:"message_id"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return Message{}, err
	}
	return Message{ID: out.MessageID, ConversationID: cv.id, Seq: int(c.messages[out.MessageID].seq)}, nil
}

// Retract withdraws a message as the seat byID, as conversation_retract
// does: its author may, and so may staff who oversee the opener.
func (c *Core) Retract(messageID, byID, reason string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	msg := c.messages[messageID]
	if msg == nil {
		return fmt.Errorf("fakecore: Retract: no message %s", messageID)
	}
	m, err := c.member("Retract", byID)
	if err != nil {
		return err
	}
	args := map[string]any{"course_id": msg.conv.course.id, "message_id": msg.id}
	if reason != "" {
		args["reason"] = reason
	}
	_, err = c.actAs(m, "conversation.retract", args)
	return err
}

// Close closes a conversation as one of its participants, byID.
func (c *Core) Close(conversationID, byID, reason string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cv := c.conversations[conversationID]
	if cv == nil {
		return fmt.Errorf("fakecore: Close: no conversation %s", conversationID)
	}
	m, err := c.member("Close", byID)
	if err != nil {
		return err
	}
	args := map[string]any{"course_id": cv.course.id, "conversation_id": cv.id}
	if reason != "" {
		args["reason"] = reason
	}
	_, err = c.actAs(m, "conversation.close", args)
	return err
}

// SetLevel sets one permission's level on a seat, as member.update_perms
// does: a delegate is not widened past its principal, and whatever its row
// says, it holds no more than its principal on any call.
func (c *Core) SetLevel(memberID, perm, lvl string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, err := c.member("SetLevel", memberID)
	if err != nil {
		return err
	}
	l, err := parseLevel(lvl)
	if err != nil {
		return err
	}
	if !validPerm(perm) {
		return fmt.Errorf("fakecore: SetLevel: there is no permission %q", perm)
	}
	if p := m.principal; p != nil && l > m.perms[perm] && l > delegateCap(p, perm) {
		return fmt.Errorf("fakecore: SetLevel: the delegate's principal holds %s at %s, so the delegate cannot hold it at %s",
			perm, delegateCap(p, perm), l)
	}
	m.perms[perm] = l
	c.flushStamped([]*event{memberEvent("member.updated", m, nil)})
	return nil
}

// PauseSeat pauses a seat: it is denied everything, and so are its
// delegates, until ResumeSeat.
func (c *Core) PauseSeat(memberID string) error {
	return c.setStatus("PauseSeat", memberID, statusActive, statusPaused)
}

// ResumeSeat resumes a paused seat.
func (c *Core) ResumeSeat(memberID string) error {
	return c.setStatus("ResumeSeat", memberID, statusPaused, statusActive)
}

func (c *Core) setStatus(control, memberID, from, to string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, err := c.member(control, memberID)
	if err != nil {
		return err
	}
	if m.status != from {
		return fmt.Errorf("fakecore: %s: the seat is %s, not %s", control, m.status, from)
	}
	m.status = to
	c.flushStamped([]*event{memberEvent("member."+map[string]string{statusPaused: "paused", statusActive: "resumed"}[to], m, nil)})
	return nil
}

// RemoveSeat removes a seat and its delegates' with it, as member.remove
// does: their open conversations are closed (seat_removed) and their
// proposals cancelled (member_removed).
func (c *Core) RemoveSeat(memberID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, err := c.member("RemoveSeat", memberID)
	if err != nil {
		return err
	}
	if m.status == statusRemoved {
		return errors.New("fakecore: RemoveSeat: the member has already been removed")
	}
	type retiree struct {
		m      *member
		reason string
	}
	gone := []retiree{{m, "removed"}}
	for _, d := range c.memberList {
		if d.principal == m && d.status != statusRemoved {
			gone = append(gone, retiree{d, "principal_removed"})
		}
	}
	var evs []*event
	for _, g := range gone {
		g.m.status = statusRemoved
		evs = append(evs, memberEvent("member.removed", g.m, map[string]any{"reason": g.reason}))
	}
	for _, cv := range c.conversationList {
		if cv.status != "open" || !slices.ContainsFunc(gone, func(g retiree) bool { return cv.opener == g.m || cv.respondent == g.m }) {
			continue
		}
		cv.status, cv.closedReason = "closed", ptr(seatRemoved)
		evs = append(evs, &event{typ: "conversation.closed", course: cv.course, subjectType: "conversation", subjectID: &cv.id,
			payload: mustJSON(map[string]any{"conversation_id": cv.id, "reason": seatRemoved})})
	}
	for _, g := range gone {
		for _, a := range c.actionList {
			if a.member != g.m || a.status != actProposed {
				continue
			}
			a.status, a.result = actCancelled, errorResult(cancellation(cancelMemberRemoved, map[string]any{"member_reason": g.reason}))
			id := a.id
			evs = append(evs, &event{typ: "action.cancelled", course: a.course, actionID: &id, subjectType: "action", subjectID: &id,
				payload: mustJSON(map[string]any{"reason": cancelMemberRemoved})})
		}
	}
	c.flushStamped(evs)
	return nil
}

// ArchiveCourse archives a course: every write in it is denied from then
// on, and everything stays readable.
func (c *Core) ArchiveCourse(courseID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	co := c.courses[courseID]
	if co == nil {
		return fmt.Errorf("fakecore: ArchiveCourse: no course %s", courseID)
	}
	co.status = statusArchived
	c.flushStamped([]*event{{typ: "course.archived", course: co, subjectType: "course", subjectID: &co.id}})
	return nil
}

// judge is who decides or reviews an action: the first seat in the
// course, in the order seated, that counts, decides actions, is not of the
// actor's party, and passes also. Nobody judges their own action, their
// agent's or their owner's.
func (c *Core) judge(a *action, also func(*member) bool) (*member, error) {
	now := c.now()
	for _, m := range c.memberList {
		if m.course == a.course && m != a.member && m.counts(now) && m.perm(permActionDecide).allowed() &&
			!sameParty(a.actor, m.actor) && also(m) {
			return m, nil
		}
	}
	return nil, errors.New("fakecore: nobody in the course may judge that action: seat someone who decides actions and is not of its actor's party")
}

func (c *Core) decideAs(control, actionID, decision string, reason *string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prop := c.actions[actionID]
	if prop == nil || prop.course == nil {
		return "", fmt.Errorf("fakecore: %s: no action %s in a course", control, actionID)
	}
	m, err := c.judge(prop, func(*member) bool { return true })
	if err != nil {
		return "", err
	}
	args := map[string]any{"course_id": prop.course.id, "action_id": prop.id, "decision": decision}
	if reason != nil {
		args["reason"] = *reason
	}
	res, err := c.actAs(m, "action.decide", args)
	if err != nil {
		return "", err
	}
	var out decideOut
	if err := json.Unmarshal(res, &out); err != nil {
		return "", err
	}
	return out.Outcome, nil
}

// Approve approves a proposal as someone who may decide it. It is carried
// out now, as its proposer, after authorizing the proposer again: the
// outcome is executed, failed (a conversation that moved on) or cancelled
// (the proposer may no longer, or it is too old).
func (c *Core) Approve(actionID string) (string, error) {
	return c.decideAs("Approve", actionID, "approve", nil)
}

// Reject rejects a proposal with a reason, which its proposer reads in
// the proposal's result.decision.reason (action_list_mine).
func (c *Core) Reject(actionID, reason string) error {
	var why *string
	if reason != "" {
		why = &reason
	}
	_, err := c.decideAs("Reject", actionID, "reject", why)
	return err
}

// Review records a person's look at an action that executed pending
// review, as action.review does: outcome reviewed or escalated. It is done
// by someone who may: the first seat that decides actions, is not of the
// actor's party, and did not escalate it. Its actor sees action.reviewed or
// action.escalated in event_list.
func (c *Core) Review(actionID, outcome string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	row := c.actions[actionID]
	if row == nil || row.course == nil {
		return fmt.Errorf("fakecore: Review: no action %s in a course", actionID)
	}
	m, err := c.judge(row, func(m *member) bool { return !c.escalatedBy(row, m.actor) })
	if err != nil {
		return err
	}
	_, err = c.actAs(m, toolActionReview, map[string]any{"course_id": row.course.id, "action_id": row.id, "outcome": outcome})
	return err
}

// Expire cancels a proposal now, as Core's sweep does one older than the
// proposal TTL: cancelled, reason proposal_expired.
func (c *Core) Expire(actionID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	prop := c.actions[actionID]
	if prop == nil || prop.status != actProposed {
		return fmt.Errorf("fakecore: Expire: no proposal %s waiting", actionID)
	}
	now := c.now()
	sweep := &action{id: newID(), actor: c.system, course: prop.course, actionType: "action.expire", targetType: "action",
		targetID: &prop.id, payload: mustJSON(map[string]any{"action_id": prop.id}), key: "job:action.expire:" + prop.id,
		authz: autonomous, status: actExecuted, reviewState: reviewNone, result: json.RawMessage(`{"done":true}`), createdAt: now, executedAt: &now}
	c.recordAction(sweep)
	prop.status, prop.result = actCancelled, errorResult(cancellation(cancelExpired, nil))
	id := prop.id
	c.flush([]*event{{typ: "action.cancelled", course: prop.course, actionID: &id, subjectType: "action", subjectID: &id,
		payload: mustJSON(map[string]any{"action_type": prop.actionType, "reason": cancelExpired, "by_action_id": sweep.id})}})
	return nil
}

// Proposals lists the course's proposals waiting for a decision, oldest
// first.
func (c *Core) Proposals(courseID string) []Proposal {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Proposal
	for _, a := range c.actionList {
		if a.course != nil && a.course.id == courseID && a.status == actProposed {
			p := Proposal{ActionID: a.id, ActionType: a.actionType, IdempotencyKey: a.key, Args: append(json.RawMessage(nil), a.payload...)}
			if a.member != nil {
				p.MemberID = a.member.id
			}
			out = append(out, p)
		}
	}
	return out
}

// MemberRecord is a seat as the fake holds it, for assertions.
type MemberRecord struct {
	ID, ActorID, Role, Status string
	// Principal is the seat a delegate is the delegate of; "" for none.
	Principal string
}

// Members are a course's seats, removed ones too, in the order they were
// made.
func (c *Core) Members(courseID string) []MemberRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []MemberRecord
	for _, m := range c.memberList {
		if m.course.id != courseID {
			continue
		}
		r := MemberRecord{ID: m.id, ActorID: m.actor.id, Role: m.role, Status: m.status}
		if m.principal != nil {
			r.Principal = m.principal.id
		}
		out = append(out, r)
	}
	return out
}

// DocumentRecord is a course's document as the fake holds it, for
// assertions.
type DocumentRecord struct {
	ID, Kind, Title string
	// Draft is a document not published: every one document.create made.
	Draft          bool
	BodyMD         string
	AuthorMemberID string
}

// Documents are a course's documents, in the order they were made: its
// canned material, then what document.create made.
func (c *Core) Documents(courseID string) []DocumentRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	co := c.courses[courseID]
	if co == nil {
		return nil
	}
	out := make([]DocumentRecord, 0, len(co.documents))
	for _, d := range co.documents {
		r := DocumentRecord{ID: d.id, Kind: d.kind, Title: d.title, Draft: d.draft, AuthorMemberID: d.authorMemberID}
		if d.bodyMD != nil {
			r.BodyMD = *d.bodyMD
		}
		out = append(out, r)
	}
	return out
}

// Messages is every message of a conversation, in order.
func (c *Core) Messages(conversationID string) []MessageRecord {
	return c.records(conversationID, func(*message) bool { return true })
}

// Answers is the respondent's messages in a conversation, in order.
func (c *Core) Answers(conversationID string) []MessageRecord {
	return c.records(conversationID, func(m *message) bool { return m.author == m.conv.respondent })
}

func (c *Core) records(conversationID string, keep func(*message) bool) []MessageRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	cv := c.conversations[conversationID]
	if cv == nil {
		return nil
	}
	var out []MessageRecord
	for _, m := range cv.messages {
		if !keep(m) {
			continue
		}
		r := MessageRecord{ID: m.id, ConversationID: cv.id, Seq: int(m.seq), AuthorMemberID: m.author.id, Body: m.body,
			Retracted: m.retraction != nil, ActionID: m.actionID, CreatedAt: m.createdAt}
		if m.inReplyTo != nil {
			r.InReplyTo = *m.inReplyTo
		}
		if a := c.actions[m.actionID]; a != nil {
			r.IdempotencyKey = a.key
		}
		out = append(out, r)
	}
	return out
}

// AddWork hands in a student's submission to the course's assignment, and
// posts a grade on it from a seat that posts grades.
func (c *Core) AddWork(courseID, studentID, body, score string) (Work, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	co := c.courses[courseID]
	if co == nil {
		return Work{}, fmt.Errorf("fakecore: AddWork: no course %s", courseID)
	}
	student := c.members[studentID]
	if student == nil || student.course != co {
		return Work{}, fmt.Errorf("fakecore: AddWork: no seat %s in the course", studentID)
	}
	var grader *member
	for _, m := range c.memberList {
		if m.course == co && m.status == statusActive && m.perm(permGradePost).allowed() {
			grader = m
			break
		}
	}
	if grader == nil {
		return Work{}, errors.New("fakecore: AddWork: nobody in the course posts grades")
	}
	now := c.now()
	s := &submission{id: newID(), assignment: co.assignments[0], student: student, body: body, createdAt: now, submittedAt: now}
	g := &grade{id: newID(), student: student, assignment: s.assignment, submission: s, grader: grader, actionID: newID(),
		score: score, createdAt: now, postedAt: now}
	co.submissions, co.grades = append(co.submissions, s), append(co.grades, g)
	return Work{SubmissionID: s.id, GradeID: g.id}, nil
}

// parseDecimal and formatDecimal are the canned gradebook's arithmetic.
func parseDecimal(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

func formatDecimal(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func ratio(score, points string) string {
	p := parseDecimal(points)
	if p == 0 {
		return "0"
	}
	return formatDecimal(parseDecimal(score) / p)
}
