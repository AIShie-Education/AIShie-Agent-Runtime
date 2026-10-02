package toolset

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
)

// A version of a document may hold several files, in order (AIShie-Core
// #49): a lecture's slides, its handout and a sample program. document_get
// gives the model every one of them (design §4, Files), each under its
// name, as a version's one file is given: its text version first where it
// is done, and otherwise the file, fetched by the runtime, as text, as a
// file part, or with why not. What one call gives is bounded as the
// question's files are in the first turn (attachments.go): each file at
// most what a result gives of it, its first part, and all of them at most
// versionResults results' worth of text and one PDF part's pages of file
// parts. A file past that, or one whose reading ran out of the answer's
// time, is named with the call that reads it: document_get with its
// FileIDArg, which gives that file alone, in parts, and its pages
// (FilePartArg, FilePagesArg), as a version of one file is given.

// versionResults is how many results' worth of text one call gives of a
// version's files in all, as the first turn gives of a question's
// (inlineResults): what a result holds is sent again with every later turn
// of the answer.
const versionResults = inlineResults

// fileEntry is one file of a version of several as the result gives it:
// its record, and its text, whole or its first part, when that is how it
// is given.
type fileEntry struct {
	*fileRecord
	FileText string `json:"file_text,omitempty"`
}

// minEntryRoom is the least room left in which a file of a version is
// still fetched to be given: less, and it is named with the call that
// reads it, unfetched.
const minEntryRoom = 1 << 10

// renderVersion is the content of document_get's result for a version of
// several files, c being Core's envelope, and the files to give beside it:
// the files in order, each as much as fits (the package's comment above),
// the rest named with the call that reads each; and whether any of them
// was given. fa asked for a part or pages without naming a file, which
// are of one file alone: they are not given, and the note says to name
// it.
func (r Runner) renderVersion(ctx context.Context, c content, ver *docVersion, fa fileArgs) (string, []*llm.File, bool) {
	var b strings.Builder
	fmt.Fprintf(&b, "the version holds %d files, in files in their order, each under its name: its text in file_text, or a file part "+
		"that follows the results under its name, or why it is not given; what one call gives of the files is bounded, so a long "+
		"file's first part is given and next_part reads the next, and a file not given here is read with next_part's call; to read "+
		"one file alone, call %s with its %s", len(ver.files), FilePartTool, FileIDArg)
	if fa.part > 0 || fa.first > 0 {
		fmt.Fprintf(&b, "; %s and %s are of one file: name it with %s", FilePartArg, FilePagesArg, FileIDArg)
	}
	c.FilesNote = b.String()
	envelope := len(encodeJSON(c))
	if envelope > r.MaxResultBytes {
		// Only a version whose own text is far longer than a file's part
		// leaves no room: the result is cut as any other, and its files
		// are read one by one.
		return r.fit(c, nil, given{}, 0), nil, false
	}
	textLeft, pagesLeft := versionResults*r.MaxResultBytes-envelope, r.partPages()
	var files []*llm.File
	for _, d := range ver.files {
		d.courseID = fa.courseID
		if ctx.Err() != nil {
			c.Files = append(c.Files, notHere(d, "the answer's time to read files ran out"))
			continue
		}
		if textLeft < minEntryRoom || pagesLeft <= 0 && r.wouldGiveFile(d) {
			c.Files = append(c.Files, notHere(d, roomTaken))
			textLeft -= len(encodeJSON(c.Files[len(c.Files)-1]))
			continue
		}
		g := r.giveFile(ctx, d, 0)
		e := r.entry(d, g)
		n := len(encodeJSON(e))
		if n > textLeft || g.file != nil && g.filePages > pagesLeft {
			e = notHere(d, roomTaken)
			n = len(encodeJSON(e))
		} else if g.file != nil {
			files = append(files, g.file)
			pagesLeft -= g.filePages
		}
		c.Files = append(c.Files, e)
		textLeft -= n
	}
	anyGiven := slices.ContainsFunc(c.Files, func(e fileEntry) bool { return e.GivenAs != givenNot })
	return encodeJSON(c), files, anyGiven
}

// roomTaken is why a file of a version is not given with the others.
const roomTaken = "the version's other files take the room this result has for files"

// wouldGiveFile reports whether d, read now, would be given as a file
// part: its text version is not done, and it is a file this model takes
// as a file (an image or a PDF, or an Office file converted to one).
func (r Runner) wouldGiveFile(d *docFile) bool {
	if d.text != nil && d.text.Status == core.TextDone {
		return false
	}
	return r.givesFile(d, mediaType(d.contentType))
}

// entry is g, what d is given as, as one of a version's files: its text
// whole where it fits a part, else its first part (pageText), and text
// beside a file part cut to a part; a record of no text as it is.
func (r Runner) entry(d *docFile, g given) fileEntry {
	e := fileEntry{fileRecord: g.rec}
	text := g.text
	if text == "" {
		return e
	}
	budget := r.partBudget()
	if !g.aside && escapedLenOf(text) > budget {
		text = r.pageText(g.rec, d, g.text, g.sections, 1)
	}
	if t, ok := fitString(text, budget+len(`""`)); ok {
		e.FileText = t
	}
	return e
}

// notHere is the record of a file of a version not given here, and why,
// with the call that reads it.
func notHere(d *docFile, why string) fileEntry {
	rec := &fileRecord{FileID: d.fileID, Position: d.position, Name: d.title, ContentType: d.contentType, ByteSize: d.byteSize,
		GivenAs: givenNot, Note: "it is not given here: " + why}
	if np := d.again(0); np != nil {
		rec.NextPart = np
		rec.Note += "; to read it, call " + np.Tool + " with next_part's arguments, which name it"
	}
	return fileEntry{fileRecord: rec}
}
