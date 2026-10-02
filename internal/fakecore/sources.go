package fakecore

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// What an answer relied on (Core's internal/tools/sources.go, docs/schema.md
// §2.8, What an answer relied on; AIShie-Core #71): conversation.answer
// takes the course materials the answer relied on, at most 20, in order,
// each a version of a material, instructions or a rubric, and, if it says
// so, one of the version's files, a page or a slide of it and a part of its
// text. What they say alone is checked as the call is read (Check); each
// must be a version document.get would show the answering seat, by id, at
// that moment, not purged, its file one of that version's, before the
// answer is posted or proposed (Validate), again as it is posted, and as
// the proposer's when a proposal is approved. They are kept with the
// answer, and an answer that gives an empty list, relying on none, is kept
// apart from one that gives none, which does not say. conversation.messages
// shows each reader an answer's sources as they may read them now: whole,
// or, for a version they may not open of a document they may, its document
// alone (other_version), or restricted, and nothing else. The fake holds
// one version of each document, so a source names another version only
// where it names one Core never gave.

// maxSources bounds the sources one answer names, as Core does.
const maxSources = 20

// maxSourceLocator bounds a page, a slide or a part, as Core does.
const maxSourceLocator = 100000

// Reasons of Core's refusals of an answer's sources.
const (
	reasonBadSource        = "bad_source"
	reasonDuplicateSource  = "duplicate_source"
	reasonTooManySources   = "too_many_sources"
	reasonSourceUnreadable = "source_unreadable"
	reasonSourcePurged     = "source_purged"
)

// sourceIn is one thing an answer relied on, as conversation.answer is
// told it.
type sourceIn struct {
	DocumentID uuid.UUID  `json:"document_id"`
	VersionID  uuid.UUID  `json:"version_id"`
	FileID     *uuid.UUID `json:"file_id,omitempty"`
	Page       *int32     `json:"page,omitempty"`
	Slide      *int32     `json:"slide,omitempty"`
	Part       *int32     `json:"part,omitempty"`
}

// sourceView is a source of an answer as one reader is shown it.
type sourceView struct {
	Restricted   bool    `json:"restricted,omitempty"`
	OtherVersion bool    `json:"other_version,omitempty"`
	DocumentID   *string `json:"document_id,omitempty"`
	Kind         *string `json:"kind,omitempty"`
	Title        *string `json:"title,omitempty"`
	VersionID    *string `json:"version_id,omitempty"`
	Seq          *int32  `json:"seq,omitempty"`
	Published    *bool   `json:"published,omitempty"`
	FileID       *string `json:"file_id,omitempty"`
	Filename     *string `json:"filename,omitempty"`
	Page         *int32  `json:"page,omitempty"`
	Slide        *int32  `json:"slide,omitempty"`
	Part         *int32  `json:"part,omitempty"`
}

// sourceField is where in a call a source is, for a refusal to say.
func sourceField(i int) string { return fmt.Sprintf("sources[%d]", i) }

// errSource refuses the source at i, saying which and why, as Core does.
func errSource(i int, reason, format string, args ...any) *apiError {
	return invalid("%s: %s", sourceField(i), fmt.Sprintf(format, args...)).
		with("field", sourceField(i)).with("index", i).with("reason", reason)
}

// checkSources is what an answer's sources say alone (Core's Check): at
// most maxSources; each naming a document and a version; a page, a slide
// or a part only with a file, a page or a slide and not both, each from
// 1; and none named twice.
func checkSources(sources []sourceIn) error {
	if len(sources) > maxSources {
		return invalid("an answer names at most %d sources; this one names %d", maxSources, len(sources)).
			with("field", "sources").with("reason", reasonTooManySources).with("max_sources", maxSources)
	}
	seen := map[string]int{}
	for i, s := range sources {
		if s.DocumentID == uuid.Nil || s.VersionID == uuid.Nil {
			return errSource(i, reasonBadSource, "a source names its document_id and its version_id")
		}
		if s.FileID == nil && (s.Page != nil || s.Slide != nil || s.Part != nil) {
			return errSource(i, reasonBadSource, "a page, a slide or a part is of a file: give file_id as well")
		}
		if s.Page != nil && s.Slide != nil {
			return errSource(i, reasonBadSource, "give a page or a slide, not both")
		}
		for _, n := range []struct {
			what string
			n    *int32
		}{{"page", s.Page}, {"slide", s.Slide}, {"part", s.Part}} {
			if n.n != nil && (*n.n < 1 || *n.n > maxSourceLocator) {
				return errSource(i, reasonBadSource, "a %s counts from 1 to %d", n.what, maxSourceLocator)
			}
		}
		k := strings.Join([]string{s.DocumentID.String(), s.VersionID.String(), idText(s.FileID), locator(s.Page),
			locator(s.Slide), locator(s.Part)}, " ")
		if j, twice := seen[k]; twice {
			return errSource(i, reasonDuplicateSource, "the same source as %s", sourceField(j))
		}
		seen[k] = i
	}
	return nil
}

func idText(id *uuid.UUID) string {
	if id == nil {
		return "-"
	}
	return id.String()
}

// locator is a page, a slide or a part as a source's key writes it.
func locator(n *int32) string {
	if n == nil {
		return "-"
	}
	return strconv.Itoa(int(*n))
}

// opens reports whether m may open doc's version now, as document.get by
// id would show it: a course's material, instructions or rubric m reads
// documents of that kind of, published to m or a draft m reads, and not
// withheld with its assignment.
func opens(doc *document, m *member) bool {
	return courseLevel(doc.kind) && m.perm(readPerm(doc.kind)).allowed() && !hiddenDraft(doc, m) && !withheld(doc, m)
}

// checkSourcesReadable holds an answer's sources to what its respondent m
// may read now (Core's Validate, and Execute): each a version document.get
// would show m, by id, of a course's material, instructions or rubric, not
// purged; each file one of that version's. A version m may not read is
// refused as one that does not exist, so that nobody learns which.
func (c *Core) checkSourcesReadable(m *member, sources []sourceIn) error {
	for i, s := range sources {
		doc := c.findDocument(m.course, s.DocumentID)
		if doc == nil || !opens(doc, m) || doc.versionID == "" || !strings.EqualFold(doc.versionID, s.VersionID.String()) {
			return errSource(i, reasonSourceUnreadable, "names no version of a course material (a material, instructions or a "+
				"rubric) that you may read")
		}
		if doc.purgedAt != nil {
			return errSource(i, reasonSourcePurged, "that version was purged: nothing of it is left to rely on")
		}
		if s.FileID != nil && doc.file(s.FileID.String()) == nil {
			return errSource(i, reasonSourceUnreadable, "file_id names no file of that version")
		}
	}
	return nil
}

// sourcesOf is an answer's sources as reader may read them now: whole
// where they may open the version, its document alone where they may open
// the document but not that version, restricted otherwise, and to every
// reader where it was purged. nil for a message that did not say.
func (c *Core) sourcesOf(reader *member, msg *message) []sourceView {
	if !msg.sourcesStated {
		return nil
	}
	out := make([]sourceView, 0, len(msg.sources))
	for _, s := range msg.sources {
		doc := c.findDocument(msg.conv.course, s.DocumentID)
		if doc == nil || doc.purgedAt != nil || !opens(doc, reader) {
			out = append(out, sourceView{Restricted: true})
			continue
		}
		v := sourceView{DocumentID: ptr(doc.id), Kind: ptr(doc.kind), Title: ptr(doc.title)}
		if !strings.EqualFold(doc.versionID, s.VersionID.String()) {
			v.OtherVersion = true
			out = append(out, v)
			continue
		}
		v.VersionID, v.Seq, v.Published = ptr(doc.versionID), ptr(int32(1)), ptr(!doc.draft)
		if s.FileID != nil {
			v.FileID = ptr(s.FileID.String())
			if f := doc.file(s.FileID.String()); f != nil {
				v.Filename = ptr(f.filename)
			}
		}
		v.Page, v.Slide, v.Part = s.Page, s.Slide, s.Part
		out = append(out, v)
	}
	return out
}

// SourceRecord is a source an answer named, as the fake holds it, for
// assertions: its ids, and its page, slide and part, 0 for none.
type SourceRecord struct {
	DocumentID, VersionID, FileID string
	Page, Slide, Part             int
}

func recordOf(s sourceIn) SourceRecord {
	r := SourceRecord{DocumentID: s.DocumentID.String(), VersionID: s.VersionID.String()}
	if s.FileID != nil {
		r.FileID = s.FileID.String()
	}
	for _, n := range []struct {
		to *int
		n  *int32
	}{{&r.Page, s.Page}, {&r.Slide, s.Slide}, {&r.Part, s.Part}} {
		if n.n != nil {
			*n.to = int(*n.n)
		}
	}
	return r
}

// withoutSources is the catalogue raw as a Core from before an answer's
// sources served it (Options.WithoutSources): conversation.answer takes
// no sources, and conversation.messages' messages carry none.
func withoutSources(raw []byte) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("fakecore: the catalogue: %w", err)
	}
	tools, _ := doc["tools"].([]any)
	found := 0
	for _, t := range tools {
		tool, _ := t.(map[string]any)
		var props map[string]any
		switch name, _ := tool["name"].(string); name {
		case toolConversationAnswer:
			in, _ := tool["input_schema"].(map[string]any)
			props, _ = in["properties"].(map[string]any)
		case "conversation.messages":
			out, _ := tool["output_schema"].(map[string]any)
			outProps, _ := out["properties"].(map[string]any)
			msgs, _ := outProps["messages"].(map[string]any)
			items, _ := msgs["items"].(map[string]any)
			props, _ = items["properties"].(map[string]any)
		}
		if _, ok := props["sources"]; ok {
			delete(props, "sources")
			found++
		}
	}
	if found != 2 {
		return nil, errors.New("fakecore: the catalogue has no sources in conversation.answer or conversation.messages to take out")
	}
	return json.Marshal(doc)
}
