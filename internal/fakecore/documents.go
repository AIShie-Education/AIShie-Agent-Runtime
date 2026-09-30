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

// fileView is a file of a version, as Core shows it (version.files).
type fileView struct {
	ID          string    `json:"id"`
	Position    int       `json:"position"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type"`
	ByteSize    int64     `json:"byte_size"`
	Checksum    *string   `json:"checksum,omitempty"`
	DownloadURL *string   `json:"download_url,omitempty"`
	Text        *textView `json:"text,omitempty"`
}

// filesOf is doc's version's files as Core lists them: document_get's with
// their download URLs at base and their text versions' bodies (full), and
// document_versions' without either. Never nil.
func filesOf(doc *document, base string, full bool) []fileView {
	out := []fileView{}
	if doc.file == nil {
		return out
	}
	ct := ""
	if doc.contentType != nil {
		ct = *doc.contentType
	}
	f := fileView{ID: doc.fileID, Position: 1, Filename: nameFromTitle(doc.title, ct), ContentType: ct, ByteSize: int64(len(doc.file))}
	if full {
		url := base + blobPath + doc.fileToken
		f.DownloadURL = &url
	}
	if doc.text != nil {
		f.Text = doc.text.view(full)
	}
	return append(out, f)
}

// nameFromTitle is a file's name made from a title, as Core makes one for
// a file named nowhere else: control and bidi characters and slashes made
// spaces, "file" for nothing, and the extension of its type added unless
// it ends with it.
func nameFromTitle(title, contentType string) string {
	name := strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' || r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 || r == 0x200e || r == 0x200f {
			return ' '
		}
		return r
	}, title))
	if name == "" {
		name = "file"
	}
	mt, _, _ := strings.Cut(contentType, ";")
	ext := map[string]string{
		"application/pdf": ".pdf", "application/msword": ".doc",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   ".docx",
		"application/vnd.ms-powerpoint":                                             ".ppt",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation": ".pptx",
		"application/vnd.ms-excel":                                                  ".xls",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         ".xlsx",
		"text/plain": ".txt", "text/markdown": ".md", "text/csv": ".csv", "text/html": ".html", "text/x-python": ".py",
		"application/json": ".json", "application/zip": ".zip", "image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif",
		"image/webp": ".webp",
	}[strings.ToLower(strings.TrimSpace(mt))]
	if ext != "" && !strings.HasSuffix(strings.ToLower(name), ext) {
		name += ext
	}
	return name
}
