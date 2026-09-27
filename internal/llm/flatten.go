package llm

import (
	"strings"
)

// FlattenToolHistory rewrites a history that holds tool calls and results
// as plain text: each assistant message's calls become a line of text in
// it, and each tool message becomes a user message holding its results.
// Reasoning parts and provider extras are dropped; file parts stay, in the
// user message. It is ForceAnswer's last resort for an API that will not
// take tool history without tools declared (Capabilities.ToolsWithHistory)
// or will not be told not to call them: the model still sees what it
// looked up, and can only answer in text.
func FlattenToolHistory(msgs []Message) []Message {
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		var parts []Part
		var text strings.Builder
		flush := func() {
			if text.Len() > 0 {
				parts = append(parts, Text(text.String()))
				text.Reset()
			}
		}
		role := m.Role
		if role == RoleTool {
			role = RoleUser
		}
		for _, p := range m.Parts {
			switch p.Type {
			case PartText:
				if text.Len() > 0 {
					text.WriteString("\n")
				}
				text.WriteString(p.Text)
			case PartToolCall:
				if text.Len() > 0 {
					text.WriteString("\n")
				}
				text.WriteString("[called " + p.Name + " with " + string(p.Args) + "]")
			case PartToolResult:
				if text.Len() > 0 {
					text.WriteString("\n")
				}
				label := "result of " + p.Name
				if p.IsError {
					label = "error from " + p.Name
				}
				text.WriteString("[" + label + "]\n" + p.Content)
			case PartFile:
				flush()
				parts = append(parts, Part{Type: PartFile, File: p.File})
			}
		}
		flush()
		if len(parts) == 0 {
			continue
		}
		// Two user messages in a row, which some APIs refuse, are merged.
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Parts = append(out[n-1].Parts, parts...)
			continue
		}
		out = append(out, Message{Role: role, Parts: parts})
	}
	return out
}
