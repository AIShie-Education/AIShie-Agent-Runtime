package fakecore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// A message of a conversation may carry files, as Core's conversation
// attachments have it (Core's internal/tools/attachment.go, docs/schema.md
// §2.8, Attachments): conversation.upload_url gives a URL to PUT the bytes
// to, served by the fake itself under blobPath, and a token, which
// conversation.open, .ask or .answer names with the file's name in
// attachments; the files are recorded with the message, in its action.
// conversation.messages lists each message's files and conversation.attachment
// gives one's download URL, to whoever may read the conversation, and to
// nobody else (the same not_found as for a file that does not exist); a
// retracted message's files are withheld as its text is (not_found, reason
// retracted), and kept. The news of a message says what it carries.

// Core's limits on a message's files, at their defaults.
const (
	attachmentMaxBytes          = 50 << 20
	attachmentsPerMessage       = 10
	attachmentConversationBytes = 500 << 20
	// uploadWindow is how long an upload URL takes its file (Core's); a
	// download URL serves it for downloadTTL, as a text version's file's.
	uploadWindow = 15 * time.Minute
	// uploadGrace is how old an upload a proposal may name (Core's
	// OrphanGrace): one older may be discarded before it is decided.
	uploadGrace      = 48 * time.Hour
	maxFilenameChars = 255
)

// upload is a file uploaded for a message, attached or not yet.
type upload struct {
	// token is the upload_token; putToken the secret part of its upload
	// URL.
	token, putToken string
	member          *member
	course          *course
	contentType     string
	expires         time.Time
	// data is what was PUT, and putAt when; nil before.
	data     []byte
	putAt    time.Time
	attached bool
}

// attachment is a file a message carries.
type attachment struct {
	id, filename, contentType, checksum string
	data                                []byte
	msg                                 *message
	// rend is its PDF rendition, nil when Core converts no such file.
	rend *rendition
}

// download is a download URL handed out for an attachment.
type download struct {
	a       *attachment
	expires time.Time
}

// attachmentIn is one file a message is to carry, as the call that writes
// it names it.
type attachmentIn struct {
	UploadToken string `json:"upload_token"`
	Filename    string `json:"filename"`
}

// attachmentView is a file a message carries, as its readers see it.
type attachmentView struct {
	ID          string    `json:"id"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type"`
	ByteSize    int64     `json:"byte_size"`
	Checksum    *string   `json:"checksum,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	// Rendition is its PDF rendition's, for an Office file: with a URL
	// that shows the PDF in conversation_attachment alone.
	Rendition *renditionView `json:"rendition,omitempty"`
}

// viewAttachment is a's view as conversation_messages lists it: its
// rendition where it stands, with no URL.
func viewAttachment(a *attachment) attachmentView {
	sum := a.checksum
	v := attachmentView{ID: a.id, Filename: a.filename, ContentType: a.contentType, ByteSize: int64(len(a.data)),
		Checksum: &sum, CreatedAt: a.msg.createdAt}
	if a.rend != nil {
		v.Rendition = a.rend.view(false, "", time.Time{})
	}
	return v
}

// Core's refusals of a message's files, worded as Core words them.
var (
	errAttachesNothing = forbid("you neither ask nor answer in this course's conversations, so you attach no file to a message here").
				with("reason", "permission_denied")
	errNoAttachment        = missing("no such attachment in this course")
	errAttachmentRetracted = missing("the message that carried this file was retracted; its files are withheld, as its text is").
				with("reason", "retracted")
	errAttachmentsNeedBody = invalid("files come with a message: give body as well as attachments").
				with("reason", "attachments_need_body")
	errBadUploadToken = invalid("upload_token is not valid").with("reason", "bad_upload_token")
	errNotYourUpload  = forbid("that upload was issued to someone else, or for something else").with("reason", "not_your_upload")
	errNotUploaded    = precondition("nothing has been uploaded to that URL yet").with("reason", "not_uploaded")
	errBadFilename    = invalid("a file's name is 1 to %d characters on one line, a name and not a path", maxFilenameChars).
				with("reason", "bad_filename")
)

func errTooLarge(size int64) *apiError {
	return precondition("the file is %d bytes; the limit is %d", size, attachmentMaxBytes).
		with("reason", "file_too_large").with("byte_size", size).with("max_bytes", attachmentMaxBytes)
}

func errConversationFull(held, adding int64) *apiError {
	return precondition("the conversation holds %d bytes of files, and these %d more would take it past its limit of %d; "+
		"start a new conversation for more", held, adding, attachmentConversationBytes).
		with("reason", "conversation_attachments_full").with("held_bytes", held).
		with("max_conversation_bytes", attachmentConversationBytes)
}

// hiddenRune is a character that has no place in a file's name: a control
// character, or one that turns the direction of the text round.
func hiddenRune(r rune) bool {
	return unicode.IsControl(r) || (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}

// checkFilename trims a file's name and holds it to Core's rule.
func checkFilename(name string) (string, error) {
	n := strings.TrimSpace(name)
	if n == "" || !utf8.ValidString(n) || utf8.RuneCountInString(n) > maxFilenameChars ||
		strings.ContainsAny(n, `/\`) || strings.ContainsFunc(n, hiddenRune) {
		return "", errBadFilename
	}
	return n, nil
}

// checkFiles holds the files a message is to carry, in m's name, to what a
// message may carry, and returns the uploads with the names they are kept
// under, claiming nothing: their number, their names, and that each is an
// upload of m's for a message in its course, uploaded, not attached, and no
// larger than a file may be. A file too large is deleted, as Core deletes
// it. old, for a proposal, refuses an upload past uploadGrace.
func (c *Core) checkFiles(m *member, files []attachmentIn, old bool) ([]*upload, []string, error) {
	if len(files) > attachmentsPerMessage {
		return nil, nil, invalid("a message carries at most %d files; this one names %d", attachmentsPerMessage, len(files)).
			with("reason", "too_many_attachments").with("max_files", attachmentsPerMessage)
	}
	names := make([]string, len(files))
	seen := map[string]bool{}
	for i, f := range files {
		if seen[f.UploadToken] {
			return nil, nil, invalid("the same upload is named twice").with("reason", "duplicate_attachment")
		}
		seen[f.UploadToken] = true
		name, err := checkFilename(f.Filename)
		if err != nil {
			return nil, nil, err
		}
		names[i] = name
	}
	ups := make([]*upload, len(files))
	for i, f := range files {
		up := c.uploads[f.UploadToken]
		switch {
		case up == nil:
			return nil, nil, errBadUploadToken
		case up.course != m.course || up.member != m:
			return nil, nil, errNotYourUpload
		case up.attached:
			return nil, nil, conflicts("that upload is already attached to a message").with("reason", "already_attached")
		case up.data == nil:
			return nil, nil, errNotUploaded
		case len(up.data) > attachmentMaxBytes:
			size := int64(len(up.data))
			up.data = nil
			return nil, nil, errTooLarge(size)
		case old && up.putAt.Before(c.now().Add(-uploadGrace)):
			return nil, nil, precondition("that upload is more than %d hours old and may be discarded before the proposal is decided; "+
				"upload the file again", int(uploadGrace.Hours())).with("reason", "upload_too_old")
		}
		ups[i] = up
	}
	return ups, names, nil
}

// heldBytes is what cv holds in files, a retracted message's included.
func heldBytes(cv *conversation) int64 {
	var n int64
	for _, msg := range cv.messages {
		for _, a := range msg.attachments {
			n += int64(len(a.data))
		}
	}
	return n
}

func sizeOfUploads(ups []*upload) int64 {
	var n int64
	for _, up := range ups {
		n += int64(len(up.data))
	}
	return n
}

// roomFor refuses files that would take cv past what a conversation holds;
// cv nil is one about to be opened.
func roomFor(cv *conversation, adding int64) error {
	if adding == 0 {
		return nil
	}
	var held int64
	if cv != nil {
		held = heldBytes(cv)
	}
	if held+adding > attachmentConversationBytes {
		return errConversationFull(held, adding)
	}
	return nil
}

// checkProposedFiles is what a message that is to wait for a decision is
// held to as it is proposed: files it may carry, young enough to outlast
// the proposal, with room for them in cv (nil: a conversation to be
// opened).
func (c *Core) checkProposedFiles(m *member, cv *conversation, files []attachmentIn) error {
	if len(files) == 0 {
		return nil
	}
	ups, _, err := c.checkFiles(m, files, true)
	if err != nil {
		return err
	}
	return roomFor(cv, sizeOfUploads(ups))
}

// attach records the uploads, checked, as the files msg carries, in order,
// with the names given; and returns what its news says of them.
func (c *Core) attach(msg *message, ups []*upload, names []string) []map[string]any {
	news := make([]map[string]any, 0, len(ups))
	queued := false
	for i, up := range ups {
		sum := sha256.Sum256(up.data)
		a := &attachment{id: newID(), filename: names[i], contentType: up.contentType, data: up.data, msg: msg,
			checksum: "sha256:" + hex.EncodeToString(sum[:])}
		up.attached = true
		msg.attachments = append(msg.attachments, a)
		c.attachments[a.id] = a
		queued = c.queueRendition(msg.conv.course, nil, a, msg.createdAt) || queued
		news = append(news, map[string]any{"id": a.id, "filename": a.filename, "content_type": a.contentType, "byte_size": len(a.data)})
	}
	if queued {
		c.queued()
	}
	return news
}

// ---------------------------------------------------------------------------
// conversation.upload_url
// ---------------------------------------------------------------------------

type uploadURLIn struct {
	inCourse
	ContentType string `json:"content_type"`
}

func conversationUploadURL() *impl {
	return define(spec[uploadURLIn]{
		gate: gateConverses,
		resolve: func(_ *Core, _ *course, _ uploadURLIn) (target, error) {
			return target{typ: "upload"}, nil
		},
		query: func(c *Core, rc *readCtx, in uploadURLIn) (any, error) {
			if !rc.member.perm(permConversationAsk).allowed() && !rc.member.perm(permConversationAnswer).allowed() {
				return nil, errAttachesNothing
			}
			if strings.TrimSpace(in.ContentType) == "" || len(in.ContentType) > 200 {
				return nil, invalid("content_type is required")
			}
			if rc.course.status == statusArchived {
				return nil, forbid("the course is archived and takes no new files").with("reason", reasonCourseArchived)
			}
			up := &upload{token: "fakeupload." + fileToken(), putToken: fileToken(), member: rc.member, course: rc.course,
				contentType: in.ContentType, expires: rc.now.Add(uploadWindow)}
			c.uploads[up.token] = up
			c.putURLs[up.putToken] = up
			return struct {
				UploadURL            string            `json:"upload_url"`
				Headers              map[string]string `json:"headers"`
				UploadToken          string            `json:"upload_token"`
				ExpiresAt            time.Time         `json:"expires_at"`
				MaxBytes             int64             `json:"max_bytes"`
				MaxFiles             int               `json:"max_files"`
				MaxConversationBytes int64             `json:"max_conversation_bytes"`
			}{rc.base + blobPath + up.putToken, map[string]string{"Content-Type": in.ContentType}, up.token, up.expires,
				attachmentMaxBytes, attachmentsPerMessage, attachmentConversationBytes}, nil
		},
	})
}

// ---------------------------------------------------------------------------
// conversation.attachment
// ---------------------------------------------------------------------------

type attachmentGetIn struct {
	inCourse
	AttachmentID uuid.UUID `json:"attachment_id"`
}

// findAttachment is the file of the course by id, or errNoAttachment.
func (c *Core) findAttachment(co *course, id uuid.UUID) (*attachment, error) {
	a := c.attachments[id.String()]
	if a == nil || a.msg.conv.course != co {
		return nil, errNoAttachment
	}
	return a, nil
}

func conversationAttachment() *impl {
	return define(spec[attachmentGetIn]{
		gate: gateConverses,
		resolve: func(c *Core, co *course, in attachmentGetIn) (target, error) {
			if _, err := c.findAttachment(co, in.AttachmentID); err != nil {
				return target{}, err
			}
			id := in.AttachmentID.String()
			return target{typ: "conversation_attachment", id: &id}, nil
		},
		query: func(c *Core, rc *readCtx, in attachmentGetIn) (any, error) {
			a, err := c.findAttachment(rc.course, in.AttachmentID)
			if err != nil {
				return nil, err
			}
			if !mayRead(rc.member, a.msg.conv, rc.now) {
				return nil, errNoAttachment
			}
			if a.msg.retraction != nil {
				return nil, errAttachmentRetracted
			}
			token := fileToken()
			expires := rc.now.Add(downloadTTL)
			c.downloads[token] = download{a: a, expires: expires}
			view := viewAttachment(a)
			if a.rend != nil {
				view.Rendition = a.rend.view(true, rc.base, rc.now)
			}
			return struct {
				attachmentView
				ConversationID string    `json:"conversation_id"`
				MessageID      string    `json:"message_id"`
				MessageSeq     int32     `json:"message_seq"`
				AuthorMemberID string    `json:"author_member_id"`
				DownloadURL    string    `json:"download_url"`
				ExpiresAt      time.Time `json:"expires_at"`
			}{view, a.msg.conv.id, a.msg.id, a.msg.seq, a.msg.author.id, rc.base + blobPath + token, expires}, nil
		},
	})
}

// ---------------------------------------------------------------------------
// The blob routes: an upload's PUT, an attachment's download
// ---------------------------------------------------------------------------

// maxUploadBytes is what the fake's own disk takes as a file arrives
// (Core's MAX_UPLOAD_BYTES at its default, which is the attachments'
// limit).
const maxUploadBytes = attachmentMaxBytes

// putBlob takes an upload's bytes, once, as Core's own disk does: the
// Content-Type the URL was issued for, at most maxUploadBytes.
func (c *Core) putBlob(w http.ResponseWriter, r *http.Request) {
	if c.putRendition(w, r, r.PathValue("token")) {
		return
	}
	c.mu.Lock()
	up := c.putURLs[r.PathValue("token")]
	now := c.now()
	c.mu.Unlock()
	if up == nil || now.After(up.expires) {
		writeError(w, forbid("the upload URL is not valid, or has expired"))
		return
	}
	if got := r.Header.Get("Content-Type"); got != up.contentType {
		writeError(w, invalid("this URL takes Content-Type %q, not %q", up.contentType, got))
		return
	}
	data, e := readUpTo(r, maxUploadBytes)
	if e != nil {
		writeError(w, e)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if up.data != nil {
		writeError(w, conflicts("this URL has been uploaded to already; a file is written once"))
		return
	}
	up.data, up.putAt = bytes.Clone(data), c.now()
	if up.data == nil {
		up.data = []byte{}
	}
	sum := sha256.Sum256(data)
	writeJSON(w, http.StatusOK, map[string]any{"byte_size": len(data), "checksum": "sha256:" + hex.EncodeToString(sum[:])})
}

// readUpTo reads an upload's body, at most most bytes, as Core's own disk
// does.
func readUpTo(r *http.Request, most int) ([]byte, *apiError) {
	data, err := io.ReadAll(io.LimitReader(r.Body, int64(most)+1))
	switch {
	case err != nil:
		return nil, invalid("the file did not arrive in full; upload it again (a file has 15 minutes to arrive)")
	case len(data) > most:
		return nil, invalid("the file is larger than %d bytes", most)
	}
	return data, nil
}

// serveAttachment serves a file conversation.attachment pointed at, as a
// download under its name; false when token names none.
func (c *Core) serveAttachment(w http.ResponseWriter, token string) bool {
	c.mu.Lock()
	d, ok := c.downloads[token]
	now := c.now()
	c.mu.Unlock()
	if !ok {
		return false
	}
	if now.After(d.expires) {
		writeError(w, forbid("the download URL is not valid, or has expired"))
		return true
	}
	h := w.Header()
	h.Set("Content-Type", d.a.contentType)
	h.Set("Content-Length", strconv.Itoa(len(d.a.data)))
	h.Set("Content-Disposition", disposition(d.a.filename))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cache-Control", "private, no-store")
	_, _ = w.Write(d.a.data)
	return true
}

// disposition is the Content-Disposition a file is served with, as Core's
// blob.Disposition makes it: a download, under its name.
func disposition(filename string) string {
	if filename != "" {
		if d := mime.FormatMediaType("attachment", map[string]string{"filename": filename}); d != "" {
			return d
		}
	}
	return "attachment"
}

// ---------------------------------------------------------------------------
// Test controls
// ---------------------------------------------------------------------------

// File is a file a person attaches to a message (AskWithFiles,
// FollowUpWithFiles), or puts in a document's version (AddFiles): its
// name, the type they declare, and its bytes.
type File struct {
	Filename    string
	ContentType string
	Data        []byte
}

// uploadAs is m uploading f for a message, as conversation.upload_url and
// its PUT do: the upload's token. The lock is held.
func (c *Core) uploadAs(m *member, f File) (string, error) {
	res, err := c.actAs(m, "conversation.upload_url", map[string]any{"course_id": m.course.id, "content_type": f.ContentType})
	if err != nil {
		return "", err
	}
	var out struct {
		UploadToken string `json:"upload_token"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", err
	}
	up := c.uploads[out.UploadToken]
	if up == nil {
		return "", fmt.Errorf("fakecore: the upload %s went missing", out.UploadToken)
	}
	up.data, up.putAt = bytes.Clone(f.Data), c.now()
	if up.data == nil {
		up.data = []byte{}
	}
	return up.token, nil
}

// uploadsAs uploads each file as m, and names them as a message's
// attachments.
func (c *Core) uploadsAs(m *member, files []File) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(files))
	for _, f := range files {
		token, err := c.uploadAs(m, f)
		if err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"upload_token": token, "filename": f.Filename})
	}
	return out, nil
}

// AskWithFiles opens a conversation from the seat openerID to respondentID
// with its first question, carrying files, as a person's front end does:
// each uploaded (conversation.upload_url, then the PUT), then named in
// conversation_open's attachments.
func (c *Core) AskWithFiles(courseID, openerID, respondentID, body string, files ...File) (Conversation, Message, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, err := c.member("AskWithFiles", openerID)
	if err != nil {
		return Conversation{}, Message{}, err
	}
	named, err := c.uploadsAs(m, files)
	if err != nil {
		return Conversation{}, Message{}, err
	}
	res, err := c.actAs(m, "conversation.open", map[string]any{"course_id": courseID, "respondent_member_id": respondentID, "body": body,
		"attachments": named})
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

// FollowUpWithFiles is the conversation's opener writing again with files,
// as conversation_ask does.
func (c *Core) FollowUpWithFiles(conversationID, body string, files ...File) (Message, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cv := c.conversations[conversationID]
	if cv == nil {
		return Message{}, fmt.Errorf("fakecore: FollowUpWithFiles: no conversation %s", conversationID)
	}
	named, err := c.uploadsAs(cv.opener, files)
	if err != nil {
		return Message{}, err
	}
	res, err := c.actAs(cv.opener, "conversation.ask", map[string]any{"course_id": cv.course.id, "conversation_id": cv.id, "body": body,
		"attachments": named})
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

// Attachments are the ids of the files the message carries, in order.
func (c *Core) Attachments(messageID string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	msg := c.messages[messageID]
	if msg == nil {
		return nil
	}
	out := make([]string, len(msg.attachments))
	for i, a := range msg.attachments {
		out[i] = a.id
	}
	return out
}
