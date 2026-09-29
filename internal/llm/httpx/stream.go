package httpx

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
)

// Stream is a provider's 2xx answer to a request that asked for
// server-sent events, read as it comes. An adapter reads its events with
// Next until io.EOF, or, when the provider answered with a body of another
// type (it did not stream: a server that ignores stream, an error in a
// 200), reads that whole with ReadAll. Either way it reads at most
// MaxResponseBytes, and Close ends it.
type Stream struct {
	Status int
	Header http.Header

	ctx  context.Context
	body io.ReadCloser
	r    *bufio.Reader
	read int64
}

// Event is one server-sent event: its type (event:, empty for the default
// message) and its data, the data: lines joined with newlines.
type Event struct {
	Name string
	Data []byte
}

// PostStream sends body to url as PostJSON does, asking for server-sent
// events. A 2xx comes back as a Stream to read; anything else is an
// *llm.Error, as from PostJSON, the answer read whole to classify it.
func PostStream(ctx context.Context, client *http.Client, url string, headers map[string]string, body []byte) (*Stream, error) {
	resp, err := send(ctx, client, http.MethodPost, url, "text/event-stream", headers, body, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer func() { _ = resp.Body.Close() }()
		_, err := readWhole(ctx, resp, headers)
		return nil, err
	}
	return &Stream{Status: resp.StatusCode, Header: resp.Header, ctx: ctx, body: resp.Body, r: bufio.NewReaderSize(resp.Body, 64<<10)}, nil
}

// NewStream reads r as the body of a 2xx answer with header: for tests, and
// for an adapter's own transport.
func NewStream(ctx context.Context, status int, header http.Header, r io.ReadCloser) *Stream {
	return &Stream{Status: status, Header: header, ctx: ctx, body: r, r: bufio.NewReaderSize(r, 64<<10)}
}

// EventStream reports whether the answer is server-sent events, as its
// Content-Type says. A provider that did not stream sent its answer whole,
// for ReadAll.
func (s *Stream) EventStream() bool {
	t, _, err := mime.ParseMediaType(s.Header.Get("Content-Type"))
	return err == nil && t == "text/event-stream"
}

// ReadAll reads the rest of the answer whole, as PostJSON would have.
func (s *Stream) ReadAll() (*Response, error) {
	data, err := io.ReadAll(io.LimitReader(s.r, MaxResponseBytes-s.read+1))
	if err != nil {
		return nil, networkError(s.ctx, err)
	}
	s.read += int64(len(data))
	if s.read > MaxResponseBytes {
		return nil, s.tooLarge()
	}
	return &Response{Status: s.Status, Header: s.Header, Body: data}, nil
}

// Close closes the answer's body. A stream closed before its end is cut
// off: the provider's connection is dropped.
func (s *Stream) Close() error { return s.body.Close() }

// Next is the next event. It returns io.EOF at the end of the answer, and
// an *llm.Error when the answer cannot be read on (the connection broke,
// the call's time ran out: ErrNetwork and ErrTimeout, which a caller may
// try again) or is larger than the runtime reads. Comments (": keep-alive")
// and events without data are passed over; an event the answer ends in
// without the blank line after it still counts, as some servers end so.
func (s *Stream) Next() (Event, error) {
	var ev Event
	var data [][]byte
	for {
		line, err := s.line()
		if err != nil {
			if errors.Is(err, io.EOF) && len(data) > 0 {
				ev.Data = bytes.Join(data, []byte("\n"))
				return ev, nil
			}
			return Event{}, err
		}
		if len(line) == 0 {
			if len(data) > 0 {
				ev.Data = bytes.Join(data, []byte("\n"))
				return ev, nil
			}
			ev = Event{}
			continue
		}
		if line[0] == ':' {
			continue
		}
		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "event":
			ev.Name = string(value)
		case "data":
			data = append(data, append([]byte(nil), value...))
		}
		// id and retry mean nothing to a call that is not resumed.
	}
}

// line is the next line of the answer without its line ending (LF or
// CRLF); io.EOF at the end, with the last line first when it has no
// ending.
func (s *Stream) line() ([]byte, error) {
	var line []byte
	for {
		chunk, err := s.r.ReadSlice('\n')
		s.read += int64(len(chunk))
		if s.read > MaxResponseBytes {
			return nil, s.tooLarge()
		}
		line = append(line, chunk...)
		switch {
		case err == nil:
			line = bytes.TrimSuffix(line[:len(line)-1], []byte("\r"))
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(line) > 0 {
				return bytes.TrimSuffix(line, []byte("\r")), nil
			}
			return nil, io.EOF
		default:
			return nil, networkError(s.ctx, err)
		}
	}
}

func (s *Stream) tooLarge() *llm.Error {
	return &llm.Error{Kind: llm.ErrServer, Status: s.Status, Message: "the response is larger than the runtime reads"}
}

// Broken is the error of a stream that ended before its answer did: the
// connection closed early, which a caller may try again as it would a
// request that failed.
func Broken(status int) *llm.Error {
	return &llm.Error{Kind: llm.ErrNetwork, Status: status, Message: "the stream ended before the answer did"}
}
