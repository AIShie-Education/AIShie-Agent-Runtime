package config

import (
	"errors"
	"fmt"
	"strings"
)

// Problem is one thing wrong with the configuration, and where it is. Load
// and Validate report every problem at once, joined (errors.Join); Problems
// takes them apart again.
type Problem struct {
	// File is the file it is in; "" for the environment.
	File string
	// Line is its line in File, when known.
	Line int
	// Agent is the agent's id, "" outside an agent document.
	Agent string
	// Path is the field, from the top of its document: agent.model.key_ref,
	// courses.<course_id>.budgets.per_asker_day, runtime.tenants.<id>.
	Path string
	// Msg says what is wrong. It never repeats a value that may be a
	// secret.
	Msg string
}

func (p *Problem) Error() string {
	var b strings.Builder
	if p.File != "" {
		b.WriteString(p.File)
		if p.Line > 0 {
			fmt.Fprintf(&b, ":%d", p.Line)
		}
		b.WriteString(": ")
	}
	if p.Agent != "" {
		fmt.Fprintf(&b, "agent %q: ", p.Agent)
	}
	if p.Path != "" {
		b.WriteString(p.Path)
		b.WriteString(": ")
	}
	b.WriteString(p.Msg)
	return b.String()
}

// Problems is every *Problem in err, which Load, Validate or ForCourse
// returned, in order.
func Problems(err error) []*Problem {
	var out []*Problem
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		if p, ok := e.(*Problem); ok { //nolint:errorlint // a Problem is never wrapped here; its joined siblings are walked below.
			out = append(out, p)
			return
		}
		if j, ok := e.(interface{ Unwrap() []error }); ok {
			for _, x := range j.Unwrap() {
				walk(x)
			}
			return
		}
		var p *Problem
		if errors.As(e, &p) {
			out = append(out, p)
		}
	}
	walk(err)
	return out
}

// issue is a problem found in an agent before it is placed: a path and
// what is wrong there.
type issue struct {
	path string
	msg  string
}

// issues collects issues under a path prefix.
type issues struct {
	prefix string
	list   []issue
}

func (is *issues) add(path, format string, args ...any) {
	is.list = append(is.list, issue{path: is.prefix + path, msg: fmt.Sprintf(format, args...)})
}

// problems places the issues in a file and an agent.
func problems(list []issue, file, agent string) []error {
	out := make([]error, len(list))
	for i, is := range list {
		out[i] = &Problem{File: file, Agent: agent, Path: is.path, Msg: is.msg}
	}
	return out
}
