package fakecore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The document writes the fake carries out: document.create, for a
// course's own documents (material, instructions, rubrics), which a
// model's write through its seat's perms is (the runtime's docs/design.md
// §4). A submission's or a grade's file is refused as not carried out here,
// and so is a version's file: a model has no upload to name. A version
// holds files, in order, each named (AIShie-Core #49): the tests put them
// there (AddFiles), document.get and document.versions list them, and
// document.file gives one again.

type documentCreateIn struct {
	inCourse
	Kind         string     `json:"kind"`
	Title        string     `json:"title"`
	SubmissionID *uuid.UUID `json:"submission_id,omitempty"`
	GradeID      *uuid.UUID `json:"grade_id,omitempty"`
	SortOrder    int32      `json:"sort_order,omitempty"`
	BodyMD       *string    `json:"body_md,omitempty"`
	Files        []struct {
		UploadToken string  `json:"upload_token"`
		Filename    *string `json:"filename,omitempty"`
	} `json:"files,omitempty"`
}

// errNoDocumentUploads refuses a version's files: the fake hands out no
// uploads for documents, so any upload a call names is none of its.
var errNoDocumentUploads = invalid("no such upload: this fake Core takes no files for documents")

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
// governs; made as a draft, with a first version when it is given text. A
// title that is only spaces is refused as the arguments are read, and a
// version's files, which only files gives since AIShie-Core #61 (the
// schema refuses upload_token), before it is carried out or proposed.
func documentCreate() *impl {
	return define(spec[documentCreateIn]{
		gate: gate{any: true, perms: []string{permDocumentWrite, permSubmissionWrite, permGradeSubmit}},
		check: func(in documentCreateIn) error {
			if strings.TrimSpace(in.Title) == "" {
				return invalid("title is required")
			}
			return nil
		},
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
		validate: func(_ *Core, _ *member, in documentCreateIn) error {
			if len(in.Files) > 0 {
				return errNoDocumentUploads
			}
			return nil
		},
		execute: func(_ *Core, ec *execCtx, in documentCreateIn) (any, error) {
			if len(in.Files) > 0 {
				return nil, errNoDocumentUploads
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

// withoutFiles is the catalogue raw as a Core from before several files to
// a version served it (Options.WithoutFiles): no document.file, and no
// file_id taken, let alone required, by document.text or the service's
// calls.
func withoutFiles(raw []byte) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("fakecore: the catalogue: %w", err)
	}
	tools, _ := doc["tools"].([]any)
	kept := tools[:0]
	found := 0
	for _, t := range tools {
		tool, _ := t.(map[string]any)
		switch name, _ := tool["name"].(string); name {
		case "document.file":
			found++
			continue
		case "document.text", "document_text.file", "document_text.renew", "document_text.complete":
			in, _ := tool["input_schema"].(map[string]any)
			props, _ := in["properties"].(map[string]any)
			if _, ok := props["file_id"]; ok {
				delete(props, "file_id")
				found++
			}
			// Required since AIShie-Core #61; before #49 there was none.
			if required, ok := in["required"].([]any); ok {
				in["required"] = slices.DeleteFunc(required, func(r any) bool { return r == "file_id" })
			}
		}
		kept = append(kept, t)
	}
	if found != 5 {
		return nil, errors.New("fakecore: the catalogue has no document.file, or no file_id in the text tools, to take out")
	}
	doc["tools"] = kept
	return json.Marshal(doc)
}

// older is v as a Core from before several files to a version gives it
// (Options.WithoutFiles): without the members that name a file.
func (c *Core) older(v any, keys ...string) any {
	if !c.opts.WithoutFiles {
		return v
	}
	var m map[string]any
	if json.Unmarshal(mustJSON(v), &m) != nil {
		return v
	}
	for _, k := range keys {
		delete(m, k)
	}
	return m
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
	// Rendition is its PDF rendition's, for an Office file: with a URL
	// that shows the PDF in document_get and document_file, where it
	// stands alone in document_versions.
	Rendition *renditionView `json:"rendition,omitempty"`
}

// maxBodies is the most text document.get gives beside a version, its
// files' text bodies together, in the order of the files.
const maxBodies = textPartBytes

// filesOf is doc's version's files as Core lists them at now: document_get's
// with their download URLs at base, their renditions' too, and the bodies
// of their text versions (full) while those given come to at most
// maxBodies, and document_versions' without any of them, and where each
// rendition stands alone. Never nil.
func filesOf(doc *document, base string, full bool, now time.Time) []fileView {
	out := []fileView{}
	given := 0
	for _, f := range doc.files {
		sum := f.checksum()
		v := fileView{ID: f.id, Position: f.position, Filename: f.filename, ContentType: f.contentType, ByteSize: int64(len(f.data)),
			Checksum: &sum}
		if full {
			url := base + blobPath + f.token
			v.DownloadURL = &url
		}
		if f.text != nil {
			body := full && f.text.status == textDone && len(f.text.body) <= textPartBytes && given+len(f.text.body) <= maxBodies
			if body {
				given += len(f.text.body)
			}
			v.Text = f.text.view(body)
		}
		if full {
			v.Rendition = f.rend.view(true, base, now)
		} else {
			v.Rendition = f.rend.stateOf()
		}
		out = append(out, v)
	}
	return out
}

// checksum is the file's, as Core's disk store works it out.
func (f *versionFile) checksum() string {
	sum := sha256.Sum256(f.data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// first is the version's first file, nil for none: what the deprecated
// fields of a version, and a text tool that names no file, are of.
func (d *document) first() *versionFile {
	if len(d.files) == 0 {
		return nil
	}
	return d.files[0]
}

// file is the version's file of id, nil for none.
func (d *document) file(id string) *versionFile {
	for _, f := range d.files {
		if f.id == id {
			return f
		}
	}
	return nil
}

// addFiles gives doc's version files, in order, each served at a URL of
// its own, and, with queue, queued for its text version where doc is a
// course's material, instructions or rubric, and for its PDF rendition
// where it is an Office file, whatever doc is; a file named nowhere is
// named from the title. Called with the lock held.
func (c *Core) addFiles(doc *document, now time.Time, queue bool, files []File) {
	for i, f := range files {
		name := f.Filename
		if name == "" {
			name = nameFromTitle(doc.title, f.ContentType)
		}
		vf := &versionFile{id: newID(), doc: doc, position: i + 1, filename: name, contentType: f.ContentType,
			data: bytes.Clone(f.Data), token: fileToken()}
		if queue && courseLevel(doc.kind) {
			vf.text = c.newText(now, false)
		}
		doc.files = append(doc.files, vf)
		c.blobs[vf.token] = vf
		if queue {
			c.queueRendition(doc.course, vf, nil, now)
		}
	}
	if queue && len(files) > 0 {
		c.queued()
	}
}

type documentFileIn struct {
	inCourse
	DocumentID uuid.UUID `json:"document_id"`
	FileID     uuid.UUID `json:"file_id"`
}

// documentFile is Core's document.file: one file of a version, with a
// fresh URL, to whoever may read its version as document.get; a file of
// no version the caller reads is not there.
func documentFile() *impl {
	return define(spec[documentFileIn]{
		gate: gate{any: true, perms: []string{permDocumentRead, permRubricRead, permSubmissionRead, permGradeRead}},
		resolve: func(c *Core, co *course, in documentFileIn) (target, error) {
			doc := c.findDocument(co, in.DocumentID)
			if doc == nil {
				return target{}, missing("no such document in this course")
			}
			return target{typ: "document", id: &doc.id, scope: ownerScope(doc), perms: []string{readPerm(doc.kind)}}, nil
		},
		query: func(c *Core, rc *readCtx, in documentFileIn) (any, error) {
			doc := c.findDocument(rc.course, in.DocumentID)
			f := doc.file(in.FileID.String())
			if f == nil || hiddenDraft(doc, rc.member) || withheld(doc, rc.member) {
				return nil, missing("no such file of this document")
			}
			now := c.now()
			sum := f.checksum()
			url := rc.base + blobPath + f.token
			out := struct {
				fileView
				DocumentID string    `json:"document_id"`
				VersionID  string    `json:"version_id"`
				Seq        int       `json:"seq"`
				Published  bool      `json:"published"`
				ExpiresAt  time.Time `json:"expires_at"`
			}{fileView: fileView{ID: f.id, Position: f.position, Filename: f.filename, ContentType: f.contentType, ByteSize: int64(len(f.data)),
				Checksum: &sum, DownloadURL: &url}, DocumentID: doc.id, VersionID: doc.versionID, Seq: 1, Published: !doc.draft,
				ExpiresAt: now.Add(downloadTTL)}
			if f.text != nil {
				out.Text = f.text.view(false)
			}
			out.Rendition = f.rend.view(true, rc.base, now)
			return out, nil
		},
	})
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
