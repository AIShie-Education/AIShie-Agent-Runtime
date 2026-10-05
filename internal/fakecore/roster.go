package fakecore

import (
	"sort"
	"time"

	"github.com/google/uuid"
)

// The reads a model makes of where students stand and what a document has
// been: submission.roster and document.versions, with Core's gates, scope
// and shapes (internal/tools/submission.go and document.go in Core).

type rosterIn struct {
	inCourse
	AssignmentID uuid.UUID `json:"assignment_id"`
	pageIn
}

// rosterEntry is a student's line in the roster, as Core's RosterEntry
// shows it: the name and seat status only to a caller who reads the
// member list.
type rosterEntry struct {
	StudentMemberID string  `json:"student_member_id"`
	DisplayName     *string `json:"display_name,omitempty"`
	MemberStatus    *string `json:"member_status,omitempty"`
	// GroupID is a group assignment's: the student's group in its set now.
	GroupID      *string    `json:"group_id,omitempty"`
	State        string     `json:"state"`
	SubmissionID *string    `json:"submission_id,omitempty"`
	Attempt      *int       `json:"attempt,omitempty"`
	SubmittedAt  *time.Time `json:"submitted_at,omitempty"`
}

// submissionRoster is Core's submission.roster: every current student the
// caller's student scope reaches (a delegate's, and its principal's), in
// order of their seats' ids, with their latest attempt at one assignment,
// those who have not started among them; the assignment checked against
// the caller's assignment scope, and one not yet published missing to a
// caller who does not write assignments.
func submissionRoster() *impl {
	return define(spec[rosterIn]{
		gate: gateSubmissions,
		resolve: func(_ *Core, co *course, in rosterIn) (target, error) {
			a, err := findOrGone(co, in.AssignmentID)
			if err != nil {
				return target{}, err
			}
			return target{typ: "assignment", id: &a.id, scope: scope{assignments: []string{a.id}}}, nil
		},
		query: func(c *Core, rc *readCtx, in rosterIn) (any, error) {
			a := findAssignment(rc.course, in.AssignmentID)
			if a.publishedAt == nil && !rc.member.perm(permAssignmentWrite).allowed() {
				return nil, missing("no such assignment in this course")
			}
			if a.groupSet != nil {
				return groupRoster(c, rc, a, in), nil
			}
			limit, after := pageLimit(in.Limit), in.after()
			var students []*member
			for _, m := range c.memberList {
				if m.course == rc.course && m.role == "student" && m.status != statusRemoved && m.id > after &&
					checkScope(rc.member, scope{students: []string{m.id}}) == "" {
					students = append(students, m)
				}
			}
			sort.Slice(students, func(i, j int) bool { return students[i].id < students[j].id })
			names := rc.member.perm(permMemberRead).allowed()
			out := struct {
				Students []rosterEntry `json:"students"`
				Next     *string       `json:"next,omitempty"`
			}{Students: []rosterEntry{}}
			for _, m := range students {
				if len(out.Students) == limit {
					break
				}
				e := rosterEntry{StudentMemberID: m.id, State: "not_started"}
				if names {
					name, status := m.actor.name, m.status
					e.DisplayName, e.MemberStatus = &name, &status
				}
				for _, s := range rc.course.submissions {
					if s.assignment == a && s.student == m {
						// The fake's work is handed in once: its latest attempt
						// is its first.
						id, attempt, at := s.id, 1, s.submittedAt
						e.State, e.SubmissionID, e.Attempt, e.SubmittedAt = "submitted", &id, &attempt, &at
					}
				}
				out.Students = append(out.Students, e)
			}
			if n := len(out.Students); n > 0 && n == limit {
				out.Next = &out.Students[n-1].StudentMemberID
			}
			return out, nil
		},
	})
}

type documentIDIn struct {
	inCourse
	DocumentID uuid.UUID `json:"document_id"`
}

// versionSummary is a version as Core's document.versions lists it.
type versionSummary struct {
	ID  string `json:"id"`
	Seq int    `json:"seq"`
	// HasFile, ContentType, ByteSize and Text are the version's one
	// file's, from a Core before several files to a version
	// (Options.WithoutFiles) alone: AIShie-Core #61 took them out.
	HasFile     *bool   `json:"has_file,omitempty"`
	ContentType *string `json:"content_type,omitempty"`
	ByteSize    *int64  `json:"byte_size,omitempty"`
	// Files are the version's files, without URLs or text; none from a
	// Core before several files to a version (Options.WithoutFiles).
	Files          *[]fileView `json:"files,omitempty"`
	AuthorMemberID string      `json:"author_member_id"`
	CreatedAt      time.Time   `json:"created_at"`
	Published      bool        `json:"published"`
	// Text is the version's one file's text version, never its body.
	Text *textView `json:"text,omitempty"`
}

// documentVersions is Core's document.versions: every version of a
// document, for a member who reads drafts and may read the document's
// kind; instructions and rubrics withheld as document.get withholds them.
// The fake's documents have one version at most.
func documentVersions() *impl {
	return define(spec[documentIDIn]{
		gate: gate{perms: []string{permDocumentReadDraft}},
		resolve: func(c *Core, co *course, in documentIDIn) (target, error) {
			doc := c.findDocument(co, in.DocumentID)
			if doc == nil {
				return target{}, missing("no such document in this course")
			}
			// Both: the right to drafts, and the right to this kind.
			return target{typ: "document", id: &doc.id, scope: ownerScope(doc), perms: []string{readPerm(doc.kind), permDocumentReadDraft}}, nil
		},
		query: func(c *Core, rc *readCtx, in documentIDIn) (any, error) {
			doc := c.findDocument(rc.course, in.DocumentID)
			if withheld(doc, rc.member) {
				return nil, missing("no such document in this course")
			}
			out := struct {
				Versions []versionSummary `json:"versions"`
			}{Versions: []versionSummary{}}
			if doc.versionID != "" {
				v := versionSummary{ID: doc.versionID, Seq: 1, AuthorMemberID: doc.authorMemberID, CreatedAt: doc.versionCreatedAt,
					Published: !doc.draft}
				files := filesOf(doc, "", false, rc.now)
				if !c.opts.WithoutFiles {
					v.Files = &files
				} else {
					has := len(files) > 0
					v.HasFile = &has
					if has {
						f := files[0]
						v.ContentType, v.ByteSize, v.Text = &f.ContentType, &f.ByteSize, f.Text
					}
				}
				out.Versions = append(out.Versions, v)
			}
			return out, nil
		},
	})
}

// rosterGroup is where one group stands on a group assignment (Core's
// RosterGroup).
type rosterGroup struct {
	GroupID             string       `json:"group_id"`
	Name                string       `json:"name"`
	Members             []workMember `json:"members"`
	State               string       `json:"state"`
	SubmissionID        *string      `json:"submission_id,omitempty"`
	Attempt             *int         `json:"attempt,omitempty"`
	SubmittedAt         *time.Time   `json:"submitted_at,omitempty"`
	SubmittedByMemberID *string      `json:"submitted_by_member_id,omitempty"`
}

// groupRoster is submission.roster on a group assignment (Core's
// groupRoster): each student the caller reaches, in order of their seats'
// ids, with their group and where the work they are part of stands
// (no_group for a student in no group of its set); and, on the first page,
// each group of the set with a member the caller reaches (every group to
// one who reaches the whole class), its members they reach, and its latest
// work they may read. The fake's group work is handed in once, never a
// draft.
func groupRoster(c *Core, rc *readCtx, a *assignment, in rosterIn) any {
	limit, after := pageLimit(in.Limit), in.after()
	names := rc.member.perm(permMemberRead).allowed()
	var students []*member
	for _, m := range c.memberList {
		if m.course == rc.course && m.role == "student" && m.status != statusRemoved && m.id > after && reaches(rc.member, m.id) {
			students = append(students, m)
		}
	}
	students = byID(students)
	out := struct {
		Students []rosterEntry `json:"students"`
		Groups   []rosterGroup `json:"groups,omitempty"`
		Next     *string       `json:"next,omitempty"`
	}{Students: []rosterEntry{}}
	for _, m := range students {
		if len(out.Students) == limit {
			break
		}
		e := rosterEntry{StudentMemberID: m.id, State: "not_started"}
		if names {
			name, status := m.actor.name, m.status
			e.DisplayName, e.MemberStatus = &name, &status
		}
		if g := a.groupSet.groupOf(m); g != nil {
			e.GroupID = &g.id
		}
		switch s := workOf(rc.course, a, m); {
		case s != nil:
			id, attempt, at := s.id, 1, s.submittedAt
			e.State, e.SubmissionID, e.Attempt, e.SubmittedAt = "submitted", &id, &attempt, &at
		case e.GroupID == nil:
			e.State = "no_group"
		}
		out.Students = append(out.Students, e)
	}
	if n := len(out.Students); n > 0 && n == limit {
		out.Next = &out.Students[n-1].StudentMemberID
	}
	if in.After != nil {
		return out
	}
	every := reachesEvery(rc.member)
	for _, g := range a.groupSet.groups {
		rg := rosterGroup{GroupID: g.id, Name: g.name, Members: []workMember{}, State: "not_started"}
		for _, m := range byName(g.liveMembers(rc.now)) {
			if !reaches(rc.member, m.id) {
				continue
			}
			wm := workMember{MemberID: m.id}
			if names {
				name := m.actor.name
				wm.DisplayName = &name
			}
			rg.Members = append(rg.Members, wm)
		}
		if len(rg.Members) == 0 && !every {
			continue
		}
		for _, s := range rc.course.submissions {
			if s.assignment == a && s.group == g && readsWork(rc.member, s) {
				id, attempt, at := s.id, 1, s.submittedAt
				rg.State, rg.SubmissionID, rg.Attempt, rg.SubmittedAt = "submitted", &id, &attempt, &at
				if s.submittedBy != nil {
					rg.SubmittedByMemberID = &s.submittedBy.id
				}
			}
		}
		out.Groups = append(out.Groups, rg)
	}
	return out
}
