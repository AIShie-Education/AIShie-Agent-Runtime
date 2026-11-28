package prompt

// Role is who wrote a message in the lightweight runner conversation.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// PartType is the kind of content a message part carries. The Go control
// plane sends text and images only; document extraction is Core's job.
type PartType string

const (
	PartText  PartType = "text"
	PartImage PartType = "image"
)

// Part is one text or image part in the POST /run contract. DataBase64 is
// used for images, never a signed URL.
type Part struct {
	Type       PartType `json:"type"`
	Text       string   `json:"text,omitempty"`
	MediaType  string   `json:"media_type,omitempty"`
	DataBase64 string   `json:"data_base64,omitempty"`
}

// Message is one conversation turn. Parts is always used by the Go control
// plane; Content accepts the compact text-only form in tests and tools.
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content,omitempty"`
	Parts   []Part `json:"parts,omitempty"`
}

// Text returns a text-only part.
func Text(s string) Part { return Part{Type: PartText, Text: s} }

// Image returns an image part whose bytes are base64 encoded.
func Image(mediaType, dataBase64 string) Part {
	return Part{Type: PartImage, MediaType: mediaType, DataBase64: dataBase64}
}

// UserText returns a user message holding s.
func UserText(s string) Message { return Message{Role: RoleUser, Parts: []Part{Text(s)}} }
