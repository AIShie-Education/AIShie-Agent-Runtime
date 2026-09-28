package redact

import (
	"context"
	"encoding"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"regexp"
)

// NewHandler wraps inner so that nothing it writes holds a secret: the
// message, and every attribute's key and value, of the record and of
// WithAttrs, and every group's name, are redacted first (§6.1). Values are
// redacted in every form a handler may print them: strings; errors, by
// their text; fmt.Stringers; byte slices, as text; slog.LogValuers, once
// resolved; groups, attribute by attribute; and any other value, as fmt
// and encoding/json would show it, down to the strings and bytes inside
// it. A value found to hold a secret is replaced by its redacted text, or
// by Placeholder when the secret is in bytes that no text form shows.
func NewHandler(inner slog.Handler, extra []*regexp.Regexp) slog.Handler {
	return &handler{inner: inner, r: New(extra)}
}

type handler struct {
	inner slog.Handler
	r     *Redactor
}

func (h *handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *handler) Handle(ctx context.Context, rec slog.Record) error {
	out := slog.NewRecord(rec.Time, rec.Level, h.r.String(rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(h.r.Attr(a))
		return true
	})
	return h.inner.Handle(ctx, out)
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	red := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		red[i] = h.r.Attr(a)
	}
	return &handler{inner: h.inner.WithAttrs(red), r: h.r}
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &handler{inner: h.inner.WithGroup(h.r.String(name)), r: h.r}
}

// Attr is a with its key and value redacted.
func (r *Redactor) Attr(a slog.Attr) slog.Attr {
	return r.attr(a, 0)
}

// maxDepth bounds how far groups and LogValuers are followed; a value that
// goes deeper is shown as Placeholder rather than risk what it holds.
const maxDepth = 32

func (r *Redactor) attr(a slog.Attr, depth int) slog.Attr {
	a.Key = r.String(a.Key)
	a.Value = r.value(a.Value, depth)
	return a
}

func (r *Redactor) value(v slog.Value, depth int) slog.Value {
	if depth > maxDepth {
		return slog.StringValue(Placeholder)
	}
	switch v.Kind() {
	case slog.KindString:
		return slog.StringValue(r.String(v.String()))
	case slog.KindLogValuer:
		return r.value(v.Resolve(), depth+1)
	case slog.KindGroup:
		attrs := v.Group()
		out := make([]slog.Attr, len(attrs))
		for i, a := range attrs {
			out[i] = r.attr(a, depth+1)
		}
		return slog.GroupValue(out...)
	case slog.KindAny:
		return r.anyValue(v)
	}
	// Numbers, booleans, durations and times hold no text.
	return v
}

// anyValue redacts a value of any other type. It is left as it is when no
// form of it holds a secret, so that handlers still see its type.
func (r *Redactor) anyValue(v slog.Value) slog.Value {
	x := v.Any()
	if x == nil {
		return v
	}
	// Bytes, json.RawMessage among them, are shown as text once redacted.
	if rv := reflect.ValueOf(x); isBytes(rv) && rv.Kind() == reflect.Slice {
		if s := string(bytesOf(rv)); r.Contains(s) {
			return slog.StringValue(r.String(s))
		}
		return v
	}
	if secretInBytes(x, r) {
		return slog.StringValue(Placeholder)
	}
	shown := fmt.Sprintf("%+v", x)
	secret := r.Contains(shown)
	switch t := x.(type) {
	case error:
		if s := safeText(t.Error); r.Contains(s) {
			return slog.StringValue(r.String(s))
		}
	case fmt.Stringer:
		if s := safeText(t.String); r.Contains(s) {
			return slog.StringValue(r.String(s))
		}
	}
	if tm, ok := x.(encoding.TextMarshaler); ok {
		if s := safeText(func() string { b, _ := tm.MarshalText(); return string(b) }); r.Contains(s) {
			secret = true
		}
	}
	if _, isErr := x.(error); !isErr {
		if s := safeText(func() string { b, _ := json.Marshal(x); return string(b) }); r.Contains(s) {
			secret = true
		}
	}
	if !secret && !secretInStrings(x, r) {
		return v
	}
	return slog.StringValue(r.String(shown))
}

// safeText calls f, turning a panic (a nil receiver, a broken method) into
// no text: the value is then judged by its other forms.
func safeText(f func() string) (s string) {
	defer func() {
		if recover() != nil {
			s = ""
		}
	}()
	return f()
}

// secretInBytes reports whether a byte slice or array anywhere inside x
// holds a secret. fmt shows such bytes as numbers and encoding/json as
// base64, forms that redaction cannot rewrite, so the whole value must go.
func secretInBytes(x any, r *Redactor) bool {
	found := false
	walk(reflect.ValueOf(x), func(v reflect.Value) {
		if !found && isBytes(v) {
			found = r.Contains(string(bytesOf(v)))
		}
	})
	return found
}

// secretInStrings reports whether a string anywhere inside x holds a
// secret, for values whose fmt and JSON forms leave some fields out.
func secretInStrings(x any, r *Redactor) bool {
	found := false
	walk(reflect.ValueOf(x), func(v reflect.Value) {
		if !found && v.Kind() == reflect.String {
			found = r.Contains(v.String())
		}
	})
	return found
}

// walkLimit bounds the elements walk visits in one value, so that logging
// a large value costs a bounded amount.
const walkLimit = 10000

// walk calls visit on v and on every value inside it, through pointers,
// interfaces, structs (unexported fields too), maps, slices and arrays, to
// a bounded depth and count, never visiting a pointer twice.
func walk(v reflect.Value, visit func(reflect.Value)) {
	seen := map[uintptr]bool{}
	count := 0
	var rec func(v reflect.Value, depth int)
	rec = func(v reflect.Value, depth int) {
		if !v.IsValid() || depth > 8 || count > walkLimit {
			return
		}
		count++
		visit(v)
		switch v.Kind() {
		case reflect.Pointer:
			if v.IsNil() || seen[v.Pointer()] {
				return
			}
			seen[v.Pointer()] = true
			rec(v.Elem(), depth+1)
		case reflect.Interface:
			rec(v.Elem(), depth+1)
		case reflect.Struct:
			for i := range v.NumField() {
				rec(v.Field(i), depth+1)
			}
		case reflect.Map:
			iter := v.MapRange()
			for iter.Next() {
				rec(iter.Key(), depth+1)
				rec(iter.Value(), depth+1)
			}
		case reflect.Slice, reflect.Array:
			if isBytes(v) {
				return
			}
			for i := range v.Len() {
				rec(v.Index(i), depth+1)
			}
		}
	}
	rec(v, 0)
}

func isBytes(v reflect.Value) bool {
	return (v.Kind() == reflect.Slice || v.Kind() == reflect.Array) && v.Type().Elem().Kind() == reflect.Uint8
}

// bytesOf reads a byte slice or array, including one in an unexported
// field, which reflect lets be read element by element.
func bytesOf(v reflect.Value) []byte {
	b := make([]byte, v.Len())
	for i := range b {
		if u := v.Index(i).Uint(); u <= math.MaxUint8 {
			b[i] = byte(u)
		}
	}
	return b
}
