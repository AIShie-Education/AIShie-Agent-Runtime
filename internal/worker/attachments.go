package worker

import (
	"context"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/prompt"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/toolset"
)

// A message may carry files (Core's conversation attachments; design §5.3,
// Attachments). The prompt announces each message's files where it was
// written (prompt.HistoryWithFiles); the question's, those of the opener's
// messages since the agent last answered, are given with it as the model's
// first turn has room for them (toolset.Runner.GiveAttachments), read
// through the pipeline a document's file goes through, before the loop's
// first call, within its wall clock; and the model is offered
// toolset.AttachmentTool, which reads any file of the conversation's
// messages, in parts. A retracted message's files are withheld by Core, and
// what the worker kept of them is dropped as the retraction is read
// (Seat.retracted).

// filesFor is the most of the answer's wall clock that giving the
// question's files may take before the model's first call.
const filesFor = 3 // a third

// carriesFiles reports whether a message up to upTo, the question, carries
// files: the answer is then offered AttachmentTool.
func carriesFiles(msgs []core.Message, upTo string) bool {
	for _, m := range msgs {
		if len(m.Attachments) > 0 && m.Retracted == nil {
			return true
		}
		if m.ID == upTo {
			break
		}
	}
	return false
}

// questionFiles are the files of the question: those of the messages up to
// upTo that are not the agent's own and come after its last, in order.
func questionFiles(msgs []core.Message, self, upTo string) []toolset.MessageFile {
	end := -1
	for i, m := range msgs {
		if m.ID == upTo {
			end = i
			break
		}
	}
	if end < 0 {
		return nil
	}
	start := 0
	for i := end; i >= 0; i-- {
		if msgs[i].AuthorMemberID == self {
			start = i + 1
			break
		}
	}
	var out []toolset.MessageFile
	for _, m := range msgs[start : end+1] {
		if m.Retracted != nil || m.AuthorMemberID == self {
			continue
		}
		for _, a := range m.Attachments {
			out = append(out, toolset.MessageFile{Attachment: a, MessageID: m.ID, MessageSeq: m.Seq})
		}
	}
	return out
}

// giveFiles makes the loop's history the conversation with the files its
// messages carry: each announced where it was written, and the question's
// given after it, as far as the first turn has room, fetched and read now,
// within a third of the answer's wall clock (shown in the draft as the
// answer reading a document). The history stays as it was when the
// question carries none.
func (l *loop) giveFiles(ctx context.Context) {
	read := l.files
	files := questionFiles(read.Messages, l.c.s.id, l.msg)
	tool := ""
	if l.set.Has(toolset.AttachmentTool) {
		tool = toolset.AttachmentTool
	}
	given := map[string][]llm.Part{}
	began := time.Now()
	if len(files) > 0 {
		fctx, cancel := context.WithDeadline(ctx, l.start.Add(l.b.WallClock()/filesFor))
		defer cancel()
		l.d.calls([]llm.Part{{Type: llm.PartToolCall, ID: "question-files", Name: toolset.AttachmentTool}})
		given = l.runner().GiveAttachments(fctx, l.c.s.course, files, tool == "")
		l.d.callsDone()
	}
	hist, err := prompt.HistoryWithFiles(read.Messages, l.c.s.id, l.msg, read.More, prompt.Files{Given: given, Tool: tool})
	if err != nil {
		return
	}
	l.history = hist
	if len(files) > 0 {
		parts := 0
		for _, ps := range given {
			for _, p := range ps {
				if p.Type == llm.PartFile {
					parts++
				}
			}
		}
		l.c.s.log.Info("the question's files are given with it", "conversation", l.c.conv, "message", l.msg, "files", len(files),
			"file_parts", parts, "took_ms", time.Since(began).Milliseconds())
	}
}

// runner is what the model's tool calls, and the question's files, are
// run with: the agent's Core client, the worker's fetcher, caches, OCR and
// conversions, the store's search index and the answer's scope of it, and
// the model's capabilities.
func (l *loop) runner() toolset.Runner {
	eff := l.c.eff
	// A PDF past what the model's provider takes as a file is given as
	// its text.
	var pdf llm.FileLimits
	if fl, ok := l.m.ad.(llm.FileLimiter); ok {
		pdf = fl.FileLimits()
	}
	r := toolset.Runner{
		Client: l.c.a.client, Files: l.c.a.s.files, Texts: l.c.a.s.texts, OCR: l.c.a.s.o.OCR, MaxParallel: eff.Tools.MaxParallelTools,
		FileInput: l.m.ad.Capabilities().FileInput, PDFLimits: pdf, Writes: l.writes, Guard: l.guard,
		Office: l.c.a.s.o.Office, PartPages: l.c.a.s.o.Env.PDFPartPages, Conversation: l.c.conv,
		Index: l.c.a.store(), Search: l.search, Searched: l.searched, Sources: l.sources,
	}
	if l.d != nil {
		r.Seen = l.d.seen
	}
	return r
}

// searched counts and logs one search of the course's materials: in ids,
// counts and timings, never its query.
func (l *loop) searched(st toolset.SearchStats) {
	m := l.c.a.s.o.Metrics
	m.SearchRequests.WithLabelValues(st.Outcome).Inc()
	for outcome, n := range map[string]int{"text": st.Indexed, "empty": st.Empty, "failed": st.Failed, "not_yet": st.NotYet} {
		if n > 0 {
			m.SearchFiles.WithLabelValues(outcome).Add(float64(n))
		}
	}
	l.c.s.log.Info("the course's materials were searched", "conversation", l.c.conv, "message", l.msg, "outcome", st.Outcome,
		"documents", st.Documents, "files", st.Files, "without_text", st.WithoutText, "indexed", st.Indexed, "empty", st.Empty, "failed", st.Failed,
		"not_yet", st.NotYet, "hits", st.Hits, "scope_read", st.ScopeRead, "took_ms", st.Took.Milliseconds())
}

// takesFilesAs reports whether two models are given files alike: both take
// file parts or neither does, within the same limits.
func takesFilesAs(a, b *model) bool {
	limits := func(m *model) llm.FileLimits {
		if fl, ok := m.ad.(llm.FileLimiter); ok {
			return fl.FileLimits()
		}
		return llm.FileLimits{}
	}
	return a.ad.Capabilities().FileInput == b.ad.Capabilities().FileInput && limits(a) == limits(b)
}
