package redact

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

type stringer struct{ key string }

func (s stringer) String() string { return "stringer holding " + s.key }

type textMarshaler struct{ key string }

func (m textMarshaler) MarshalText() ([]byte, error) { return []byte("text " + m.key), nil }

type jsonOnly struct{ key string }

func (j jsonOnly) MarshalJSON() ([]byte, error) { return json.Marshal(map[string]string{"k": j.key}) }

type valuer struct{ key string }

func (v valuer) LogValue() slog.Value {
	return slog.GroupValue(slog.String("inner", v.key), slog.Int("n", 1))
}

type nested struct {
	Name   string
	Header map[string][]string
	Raw    []byte
	hidden string
	Next   *nested
}

type panicky struct{}

func (*panicky) String() string { panic("broken") }

func handlers(buf *bytes.Buffer) map[string]slog.Handler {
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	return map[string]slog.Handler{
		"json": slog.NewJSONHandler(buf, opts),
		"text": slog.NewTextHandler(buf, opts),
	}
}

func TestHandlerRedactsEveryForm(t *testing.T) {
	self := &nested{Name: "loop"}
	self.Next = self
	for name := range handlers(&bytes.Buffer{}) {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			log := slog.New(NewHandler(handlers(&buf)[name], nil))
			log.Info("calling with "+coreToken,
				slog.String("token", coreToken),
				slog.String("header", "Authorization: Bearer "+openaiKey),
				slog.Any("err", fmt.Errorf("provider refused %s: %w", anthropic, errors.New("401"))),
				slog.Any("stringer", stringer{googleKey}),
				slog.Any("pstringer", &stringer{googleKey}),
				slog.Any("text", textMarshaler{awsKey}),
				slog.Any("json", jsonOnly{inviteToken}),
				slog.Any("bytes", []byte("body "+coreToken)),
				slog.Any("raw", json.RawMessage(`{"key":"`+openaiKey+`"}`)),
				slog.Any("struct", nested{Name: "n", Header: map[string][]string{"X-Api-Key": {anthropic}}, hidden: googleKey}),
				slog.Any("struct bytes", nested{Name: "n", Raw: []byte(coreToken)}),
				slog.Any("ptr", &nested{Next: &nested{hidden: awsKey}}),
				slog.Any("map", map[string]any{"k": []string{inviteToken}}),
				slog.Any("valuer", valuer{openaiKey}),
				slog.Group("grp", slog.String("deep", coreToken), slog.Group("deeper", slog.Any("e", errors.New(googleKey)))),
				slog.String(coreToken, "as a key"),
				slog.Any("loop", self),
				slog.Any("panics", &panicky{}),
				slog.Int("count", 3),
				slog.Duration("took", time.Second),
			)
			out := buf.String()
			assertClean(t, out)
			for _, b := range []string{coreToken} {
				if strings.Contains(out, base64.StdEncoding.EncodeToString([]byte(b))[:20]) {
					t.Fatalf("base64 of a token survived:\n%s", out)
				}
			}
			for _, want := range []string{"[redacted]", "count", "took", "loop"} {
				if !strings.Contains(out, want) {
					t.Fatalf("%q missing from:\n%s", want, out)
				}
			}
		})
	}
}

func TestHandlerWithAttrsAndGroups(t *testing.T) {
	for name := range handlers(&bytes.Buffer{}) {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			h := NewHandler(handlers(&buf)[name], []*regexp.Regexp{regexp.MustCompile(`school-secret-\d+`)})
			log := slog.New(h).
				With("agent_token", coreToken, "extra", "school-secret-7").
				WithGroup("call " + awsKey).
				With(slog.Any("err", errors.New("x-api-key: "+anthropic)))
			log.Warn("done", "key", openaiKey)
			out := buf.String()
			assertClean(t, out)
			if strings.Contains(out, "school-secret-7") {
				t.Fatalf("extra pattern survived:\n%s", out)
			}
			if !strings.Contains(out, "done") {
				t.Fatalf("message lost:\n%s", out)
			}
		})
	}
}

func TestHandlerKeepsCleanValues(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewHandler(slog.NewJSONHandler(&buf, nil), nil))
	type ids struct {
		Agent  string `json:"agent"`
		Course string `json:"course"`
	}
	log.Info("answered", slog.Any("ids", ids{"agt_1", "c1"}), slog.Any("raw", json.RawMessage(`{"n":1}`)))
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if m, ok := got["ids"].(map[string]any); !ok || m["agent"] != "agt_1" {
		t.Fatalf("a clean value should keep its structure: %s", buf.String())
	}
}

func TestHandlerEnabledAndEmptyGroup(t *testing.T) {
	inner := slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelWarn})
	h := NewHandler(inner, nil)
	if h.Enabled(context.Background(), slog.LevelInfo) || !h.Enabled(context.Background(), slog.LevelError) {
		t.Fatal("Enabled should follow the inner handler")
	}
	if h.WithGroup("") != h {
		t.Fatal("WithGroup(\"\") returns the receiver")
	}
}

type deepValuer struct{ n int }

func (d deepValuer) LogValue() slog.Value {
	return slog.GroupValue(slog.Any("next", deepValuer{d.n + 1}), slog.String("k", coreToken))
}

func TestHandlerBoundsDepth(t *testing.T) {
	var buf bytes.Buffer
	slog.New(NewHandler(slog.NewJSONHandler(&buf, nil), nil)).Info("deep", slog.Any("v", deepValuer{}))
	assertClean(t, buf.String())
}

type unexportedBytes struct {
	name string
	raw  []byte
}

func TestHandlerBytes(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewHandler(slog.NewJSONHandler(&buf, nil), nil))
	log.Info("x",
		slog.Any("raw", json.RawMessage(`{"key":"`+openaiKey+`"}`)),
		slog.Any("hidden", unexportedBytes{name: "n", raw: []byte("token " + coreToken)}),
		slog.Any("array", [4]byte{'a', 'b', 'c', 'd'}),
	)
	assertClean(t, buf.String())
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// Bytes holding a secret are shown as their text, redacted; a value
	// with such bytes deeper inside goes whole.
	if got["raw"] != `{"key":"[redacted]"}` || got["hidden"] != Placeholder {
		t.Fatalf("%s", buf.String())
	}
}

type formatOnly struct{ k string }

func (f formatOnly) Format(s fmt.State, _ rune) { fmt.Fprintf(s, "formatted(%s)", f.k) }

type quietError struct{ Secret string }

func (quietError) Error() string { return "an error" }

type (
	level1  struct{ Next level2 }
	level2  struct{ Next level3 }
	level3  struct{ Next level4 }
	level4  struct{ Next level5 }
	level5  struct{ Next level6 }
	level6  struct{ Next level7 }
	level7  struct{ Next level8 }
	level8  struct{ Next level9 }
	level9  struct{ Next level10 }
	level10 struct{ Key string }
)

// Every token and key shape, in every form an attribute may take, through
// both of slog's handlers: nothing of it is written.
func TestHandlerEveryShapeInEveryForm(t *testing.T) {
	shapes := map[string]string{
		"core token":    coreToken,
		"invitation":    inviteToken,
		"openai":        "sk-proj-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789",
		"anthropic":     "sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789",
		"deepseek":      "sk-0123456789abcdef0123456789abcdef",
		"google":        googleKey,
		"aws":           awsKey,
		"aws temporary": awsTempKey,
	}
	forms := func(tok string) []slog.Attr {
		s := tok
		p := &s
		return []slog.Attr{
			slog.String("string", tok),
			slog.String("run into a word", "apikey"+tok),
			slog.String("header", "Authorization: Bearer "+tok),
			slog.String("json header", `{"x-api-key":["`+tok+`"]}`),
			slog.Any("http header", http.Header{"X-Api-Key": {tok}}),
			slog.Any("url", &url.URL{Scheme: "https", Host: "h", RawQuery: "key=" + tok}),
			slog.Any("stringer", stringer{tok}),
			slog.Any("text marshaler", textMarshaler{tok}),
			slog.Any("json marshaler", jsonOnly{tok}),
			slog.Any("log valuer", valuer{tok}),
			slog.Any("formatter", formatOnly{tok}),
			slog.Any("error with a field", quietError{tok}),
			slog.Any("joined errors", errors.Join(errors.New("a"), fmt.Errorf("b %s", tok))),
			slog.Any("deep struct", level1{level2{level3{level4{level5{level6{level7{level8{level9{level10{tok}}}}}}}}}}),
			slog.Any("slice", []any{1, tok}),
			slog.Any("attrs", []slog.Attr{slog.String("in", tok)}),
			slog.Any("map key", map[string]int{tok: 1}),
			slog.Any("raw json", json.RawMessage(`"`+tok+`"`)),
			slog.Any("bytes", []byte(tok)),
			slog.Any("array", [1]string{tok}),
			slog.Any("pointer to pointer", &p),
			slog.Group("group", slog.Group(tok, slog.String(tok, tok))),
		}
	}
	for shape, tok := range shapes {
		for name := range handlers(&bytes.Buffer{}) {
			for _, a := range forms(tok) {
				var buf bytes.Buffer
				slog.New(NewHandler(handlers(&buf)[name], nil)).With(tok, tok).WithGroup(tok).LogAttrs(context.Background(), slog.LevelInfo, tok, a)
				if out := buf.String(); strings.Contains(out, tok[len(tok)-12:]) {
					t.Errorf("%s, %s handler, %s: %s", shape, name, a.Key, out)
				}
			}
		}
	}
}
