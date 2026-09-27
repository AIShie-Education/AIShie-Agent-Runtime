package openaichat

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
)

// numericStatusFix is a transport that makes one kind of error body
// readable to httpx.Classify: Azure's, whose error.status is a number
// where httpx expects a string, so that the whole error, its code and
// message with it, fails to decode and a content filter reads as a bad
// request. It rewrites that one field as a string and leaves every other
// answer alone. It can go once httpx reads a numeric status itself.
type numericStatusFix struct {
	base http.RoundTripper
}

// maxErrorBody bounds what is read of an error body to be rewritten; a
// larger one passes as it is.
const maxErrorBody = 1 << 20

func (t numericStatusFix) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp.StatusCode < 400 {
		return resp, err
	}
	head, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody+1))
	if err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	body := io.Reader(bytes.NewReader(head))
	if len(head) > maxErrorBody {
		body = io.MultiReader(body, resp.Body)
	} else if fixed, ok := stringifyStatus(head); ok {
		body = bytes.NewReader(fixed)
		resp.ContentLength = int64(len(fixed))
		resp.Header.Del("Content-Length")
	}
	resp.Body = struct {
		io.Reader
		io.Closer
	}{body, resp.Body}
	return resp, nil
}

// stringifyStatus rewrites {"error":{"status":400,…}} with the status as a
// string, and reports whether it did.
func stringifyStatus(body []byte) ([]byte, bool) {
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return nil, false
	}
	var e map[string]json.RawMessage
	if json.Unmarshal(top["error"], &e) != nil {
		return nil, false
	}
	status := bytes.TrimSpace(e["status"])
	var n json.Number
	if len(status) == 0 || status[0] == '"' || json.Unmarshal(status, &n) != nil {
		return nil, false
	}
	e["status"] = json.RawMessage(strconv.Quote(n.String()))
	errObj, err := marshalObject(e)
	if err != nil {
		return nil, false
	}
	top["error"] = errObj
	out, err := marshalObject(top)
	if err != nil {
		return nil, false
	}
	return out, true
}

// withStatusFix is client with numericStatusFix around its transport.
func withStatusFix(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c := *client
	c.Transport = numericStatusFix{base: base}
	return &c
}
