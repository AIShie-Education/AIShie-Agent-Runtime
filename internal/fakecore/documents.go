package fakecore

import (
	"strings"

	"github.com/google/uuid"
)

// The document writes the fake carries out: document.create, for a
// course's own documents (material, instructions, rubrics), which a
// model's write through its seat's perms is (the runtime's docs/design.md
// §4). A submission's or a grade's file is refused as not carried out here.

type documentCreateIn struct {
	inCourse
	Kind         string     `json:"kind"`
	Title        string     `json:"title"`
	SubmissionID *uuid.UUID `json:"submission_id,omitempty"`
	GradeID      *uuid.UUID `json:"grade_id,omitempty"`
	SortOrder    int32      `json:"sort_order,omitempty"`
	BodyMD       *string    `json:"body_md,omitempty"`
	UploadToken  *string    `json:"upload_token,omitempty"`
}

type documentCreateOut struct {
	DocumentID string  `json:"document_id"`
	VersionID  *string `json:"version_id,omitempty"`
}

// writePerm is the permission that writes a document of kind, as Core's
// document.go has it: a submission's file is its student's work, a
// feedback file a grade's, and the rest the course's.
func writePerm(kind string) string {
	switch kind {
	case kindSubmission:
		return permSubmissionWrite
	case kindFeedback:
		return permGradeSubmit
	}
	return permDocumentWrite
}

// documentCreate is Core's document.create for a course's own documents:
// gated on any of the three writes, the kind then naming the one that
// governs; made as a draft, with a first version when it is given text; a
// title that is only spaces fails when it is carried out.
func documentCreate() *impl {
	return define(spec[documentCreateIn]{
		gate: gate{any: true, perms: []string{permDocumentWrite, permSubmissionWrite, permGradeSubmit}},
		resolve: func(_ *Core, _ *course, in documentCreateIn) (target, error) {
			t := target{typ: "document", perms: []string{writePerm(in.Kind)}}
			switch {
			case courseLevel(in.Kind):
				if in.SubmissionID != nil || in.GradeID != nil {
					return t, invalid("%s does not belong to a submission or a grade", in.Kind)
				}
			case in.Kind == kindSubmission && (in.SubmissionID == nil || in.GradeID != nil):
				return t, invalid("a submission file needs submission_id, and only that")
			case in.Kind == kindFeedback && (in.GradeID == nil || in.SubmissionID != nil):
				return t, invalid("a feedback file needs grade_id, and only that")
			case in.Kind == kindSubmission || in.Kind == kindFeedback:
				return t, forbid("a %s file is not something this fake Core carries out", in.Kind).with("reason", "not_implemented")
			default:
				return t, invalid("kind must be material, instructions, rubric, submission or feedback")
			}
			return t, nil
		},
		execute: func(_ *Core, ec *execCtx, in documentCreateIn) (any, error) {
			if strings.TrimSpace(in.Title) == "" {
				return nil, invalid("title is required")
			}
			if in.UploadToken != nil {
				return nil, invalid("no such upload: this fake Core takes no files")
			}
			doc := &document{id: newID(), kind: in.Kind, title: in.Title, course: ec.course, sortOrder: int(in.SortOrder),
				createdAt: ec.now, draft: true, authorMemberID: ec.member.id}
			out := documentCreateOut{DocumentID: doc.id}
			if in.BodyMD != nil && *in.BodyMD != "" {
				doc.versionID, doc.bodyMD, doc.versionCreatedAt = newID(), in.BodyMD, ec.now
				out.VersionID = &doc.versionID
			}
			ec.course.documents = append(ec.course.documents, doc)
			return out, nil
		},
	})
}
