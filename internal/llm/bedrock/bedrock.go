// Package bedrock is the bedrock_converse adapter: AWS Bedrock's Converse
// API (Core's docs/agent-runtime.md §3, the Bedrock column), one POST to
// /model/{modelId}/converse, signed with SigV4 or carrying a Bedrock API
// key.
//
// The translation is the package's own (request.go, response.go), with
// what each model behind Bedrock takes (model.go); AWS's SDK is used only
// for what a library does better than a page of code here: SigV4 and the
// default credential chain.
package bedrock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/smithy-go/encoding/httpbinding"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/httpx"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/toolschema"
)

// signingService is the SigV4 service name of Bedrock's runtime endpoints.
const signingService = "bedrock"

// regionPattern is what an AWS region looks like. The region is put into
// the default host name, so anything else is refused rather than allowed
// to reshape the URL.
var regionPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Adapter is the bedrock_converse adapter. It holds no state between calls
// and is safe for concurrent use.
type Adapter struct {
	model    string
	provider string
	endpoint string
	region   string
	maker    string
	caps     llm.Capabilities
	dialect  toolschema.Dialect
	params   llm.Params
	effort   string
	family   family
	headers  map[string]string
	client   *http.Client

	// apiKey is a Bedrock API key, sent as a bearer. When it is empty,
	// every request is signed with SigV4 using creds.
	apiKey string
	creds  aws.CredentialsProvider
	signer *v4.Signer
	// now is the signing time; tests fix it.
	now func() time.Time
}

// New builds the adapter from cfg. With cfg.APIKey set, requests carry it
// as a Bedrock API key; otherwise they are signed with SigV4, with
// credentials from AWS's default chain (the environment, the shared files,
// web identity, ECS, EC2's instance role), looked up when first needed and
// cached. cfg.Region falls back to the chain's region. cfg.BaseURL, when
// set, replaces https://bedrock-runtime.{region}.amazonaws.com: a VPC
// endpoint, a proxy, a test server.
func New(cfg llm.Config) (*Adapter, error) {
	return build(cfg, nil)
}

// NewWithCredentials is New with SigV4 credentials given rather than looked
// up: for tests, and for callers that hold credentials of their own, such as
// a role assumed per tenant. cfg.APIKey must be empty, since a request is
// either signed or carries a key.
func NewWithCredentials(cfg llm.Config, creds aws.CredentialsProvider) (*Adapter, error) {
	if creds == nil {
		return nil, errors.New("bedrock: no credentials given")
	}
	if cfg.APIKey != "" {
		return nil, errors.New("bedrock: both an API key and SigV4 credentials given; a request uses one")
	}
	return build(cfg, creds)
}

func build(cfg llm.Config, creds aws.CredentialsProvider) (*Adapter, error) {
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("bedrock: no model id")
	}
	sigv4 := cfg.APIKey == ""
	region := cfg.Region
	if region == "" || (sigv4 && creds == nil) {
		awsCfg, err := loadAWSConfig(region)
		switch {
		case err != nil && sigv4:
			return nil, err
		case err == nil:
			// Under an API key the configuration is read only for its
			// region, and one that does not read leaves the region unset.
			if region == "" {
				region = awsCfg.Region
			}
			if sigv4 && creds == nil {
				creds = awsCfg.Credentials
			}
		}
	}
	if sigv4 && creds == nil {
		return nil, errors.New("bedrock: no API key, and no AWS credentials to sign with")
	}
	// SigV4 names the region in every signature, and the default host
	// holds it; only a bearer to an explicit base URL goes without.
	if region == "" && (sigv4 || cfg.BaseURL == "") {
		return nil, errors.New("bedrock: no region: set the model's region, or AWS_REGION")
	}
	if region != "" && !regionPattern.MatchString(region) {
		return nil, fmt.Errorf("bedrock: %q is not an AWS region", region)
	}
	base, err := baseURL(cfg.BaseURL, region)
	if err != nil {
		return nil, err
	}

	provider := cfg.Provider
	if provider == "" {
		provider = llm.DetectProvider(llm.AdapterBedrockConverse, cfg.BaseURL)
	}
	caps, dialect := llm.Defaults(llm.AdapterBedrockConverse, provider)
	caps = cfg.Capabilities.Apply(caps)
	// Converse has no tool choice none, so no override can give it one:
	// ForceAnswer always leaves toolConfig out and flattens the history.
	caps.ToolChoiceNone = false
	caps.ToolsWithHistory = true
	if cfg.Dialect != "" {
		if !cfg.Dialect.Valid() {
			return nil, fmt.Errorf("bedrock: unknown schema dialect %q", cfg.Dialect)
		}
		dialect = cfg.Dialect
	}

	if creds != nil {
		if _, cached := creds.(*aws.CredentialsCache); !cached {
			creds = aws.NewCredentialsCache(creds)
		}
	}
	return &Adapter{
		model:    cfg.Model,
		provider: provider,
		endpoint: httpx.Join(base, "model/"+escapeModelID(cfg.Model)+"/converse"),
		region:   region,
		maker:    llm.MakerOf(llm.AdapterBedrockConverse, base, cfg.Model),
		caps:     caps,
		dialect:  dialect,
		params:   cfg.Params,
		effort:   cfg.Reasoning.Effort,
		family:   familyOf(cfg.Model),
		headers:  canonicalHeaders(cfg.Headers),
		client:   cfg.HTTPClient,
		apiKey:   cfg.APIKey,
		creds:    creds,
		signer:   v4.NewSigner(),
		now:      time.Now,
	}, nil
}

// canonicalHeaders are the configured headers under their canonical names,
// so that the key's Authorization, set after them, is the one sent however
// a configured header is spelt.
func canonicalHeaders(h map[string]string) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[http.CanonicalHeaderKey(k)] = v
	}
	return out
}

// loadAWSConfig reads AWS's shared configuration and environment for the
// region and the credential chain. Credentials are not fetched here: the
// chain is asked on the first signed call.
func loadAWSConfig(region string) (aws.Config, error) {
	var opts []func(*awsconfig.LoadOptions) error
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	c, err := awsconfig.LoadDefaultConfig(context.Background(), opts...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("bedrock: reading the AWS configuration: %w", err)
	}
	return c, nil
}

// baseURL is the configured base, or Bedrock's runtime host in region.
func baseURL(configured, region string) (string, error) {
	if configured == "" {
		suffix := "amazonaws.com"
		if strings.HasPrefix(region, "cn-") {
			suffix = "amazonaws.com.cn"
		}
		return "https://bedrock-runtime." + region + "." + suffix, nil
	}
	u, err := url.Parse(configured)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("bedrock: the base URL %q is not an http(s) URL with a host", configured)
	}
	return strings.TrimRight(configured, "/"), nil
}

// escapeModelID escapes a model id, an inference profile or an ARN as one
// path segment, every byte but AWS's unreserved ones: the path the AWS SDK
// sends, so that "anthropic.claude-…-v1:0" and ".../inference-profile/…"
// reach the same resource and the signature covers what is sent.
func escapeModelID(id string) string { return httpbinding.EscapePath(id, true) }

// Name is bedrock_converse.
func (a *Adapter) Name() string { return llm.AdapterBedrockConverse }

// Provider is bedrock, unless the configuration names another.
func (a *Adapter) Provider() string { return a.provider }

// Model is the configured model id, inference profile or ARN.
func (a *Adapter) Model() string { return a.model }

// Maker is the adapter, the endpoint's base and the model: reasoning is
// replayed only to the same model at the same endpoint.
func (a *Adapter) Maker() string { return a.maker }

// Dialect is the schema dialect tools must be given in.
func (a *Adapter) Dialect() toolschema.Dialect { return a.dialect }

// Capabilities are the adapter's, with the agent's overrides.
func (a *Adapter) Capabilities() llm.Capabilities { return a.caps }

// FileLimits are Converse's on a document (maxDocumentBytes) and the
// pages Anthropic's models, which read PDFs through it, take in one.
func (a *Adapter) FileLimits() llm.FileLimits {
	return llm.FileLimits{PDFBytes: maxDocumentBytes, PDFPages: 100}
}

// Call makes one Converse call.
func (a *Adapter) Call(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	wire, err := a.translate(req)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, &llm.Error{Kind: llm.ErrBadRequest, Message: llm.Clip("bedrock: encoding the request: " + err.Error())}
	}
	var creds aws.Credentials
	var sign func(*http.Request) error
	if a.apiKey == "" {
		if creds, err = a.creds.Retrieve(ctx); err != nil {
			return nil, credentialsError(ctx, err)
		}
		sign = a.signFunc(body, creds)
	}
	client, seen := capturing(a.client)
	resp, err := httpx.Do(ctx, client, http.MethodPost, a.endpoint, a.requestHeaders(), body, sign)
	if err != nil {
		var e *llm.Error
		if errors.As(err, &e) {
			if e.Status != 0 {
				classify(e, seen.header)
			}
			e.Message = scrub(e.Message, a.apiKey, creds.SessionToken, creds.SecretAccessKey, creds.AccessKeyID)
		}
		return nil, err
	}
	out, err := a.parse(resp.Body)
	if err != nil {
		return nil, httpx.DecodeError(err)
	}
	out.RequestID = resp.Header.Get("X-Amzn-Requestid")
	return out, nil
}

// credentialsError is a failure to get credentials from AWS's chain: the
// call's own deadline or cancellation when that is what stopped it, a
// network error when the chain could not reach its source (STS, the
// instance's metadata), and otherwise no credentials to sign with.
func credentialsError(ctx context.Context, err error) *llm.Error {
	var ne net.Error
	switch {
	case ctx.Err() != nil:
		return &llm.Error{Kind: llm.ErrTimeout, Message: "the call ended while AWS credentials were fetched"}
	case errors.As(err, &ne):
		return &llm.Error{Kind: llm.ErrNetwork, Message: llm.Clip("retrieving AWS credentials: " + err.Error())}
	}
	return &llm.Error{Kind: llm.ErrAuth, Message: llm.Clip("retrieving AWS credentials: " + err.Error())}
}

// requestHeaders are the configured extra headers and, under an API key,
// the key, set last so that no configured header replaces it.
func (a *Adapter) requestHeaders() map[string]string {
	h := make(map[string]string, len(a.headers)+1)
	maps.Copy(h, a.headers)
	if a.apiKey != "" {
		// A Bedrock API key is a bearer token (AWS_BEARER_TOKEN_BEDROCK);
		// the handout marks this [UNVERIFIED]. SigV4 is the default.
		h["Authorization"] = "Bearer " + a.apiKey
	}
	return h
}

// signFunc signs a request carrying body with SigV4 under creds. It runs
// after every header is set, so all of them are signed.
func (a *Adapter) signFunc(body []byte, creds aws.Credentials) func(*http.Request) error {
	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])
	return func(r *http.Request) error {
		return a.signer.SignHTTP(r.Context(), creds, r, payloadHash, signingService, a.region, a.now().UTC())
	}
}

// seenHeader holds the header of the last response a call received.
type seenHeader struct {
	base   http.RoundTripper
	header http.Header
}

func (s *seenHeader) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := s.base.RoundTrip(r)
	if resp != nil {
		s.header = resp.Header
	}
	return resp, err
}

// capturing returns a copy of client, for one call, that keeps the
// response's header: httpx turns a refusal into an *llm.Error from its
// status and body, but AWS names the error in x-amzn-ErrorType. The copy
// shares the original's transport, and so its connections.
func capturing(client *http.Client) (*http.Client, *seenHeader) {
	if client == nil {
		client = http.DefaultClient
	}
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	seen := &seenHeader{base: base}
	c := *client
	c.Transport = seen
	return &c, seen
}

// classify refines httpx's classification of a refusal by the AWS error
// type: the x-amzn-ErrorType header, else the body's __type, which httpx
// took as the code.
func classify(e *llm.Error, header http.Header) {
	typ := errorType(header.Get("X-Amzn-Errortype"))
	if typ == "" {
		typ = errorType(e.Code)
	}
	if typ == "" {
		return
	}
	e.Code = typ
	switch typ {
	case "ThrottlingException":
		e.Kind = llm.ErrRateLimited
	case "ServiceQuotaExceededException":
		// AWS: "You can resubmit your request later". A quota per minute
		// or per day passes; the loop's backoff and the fallback model
		// are the right answer, not giving up.
		e.Kind = llm.ErrRateLimited
	case "ServiceUnavailableException", "ModelNotReadyException":
		e.Kind = llm.ErrOverloaded
	case "AccessDeniedException", "UnrecognizedClientException", "ExpiredTokenException",
		"InvalidSignatureException", "IncompleteSignatureException", "MissingAuthenticationTokenException":
		e.Kind = llm.ErrAuth
	case "ValidationException":
		if tooLong(e.Message) {
			e.Kind = llm.ErrContextOverflow
		} else if e.Kind != llm.ErrContentFilter {
			e.Kind = llm.ErrBadRequest
		}
	case "ModelTimeoutException":
		e.Kind = llm.ErrTimeout
	case "ModelErrorException", "InternalServerException":
		e.Kind = llm.ErrServer
	}
}

// echoedRequest is the part of an AWS signature error that repeats the
// request as AWS would have signed it: its canonical string and string to
// sign list every signed header's value, the session token among them.
var echoedRequest = regexp.MustCompile(`(?is)\s*The (?:canonical string|string-to-sign)\b.*$`)

// secretField is a header or parameter that carries a credential, with
// its value to the end of the line or quotation, in whatever an error
// repeats of a request.
var secretField = regexp.MustCompile(`(?i)(x-amz-security-token|authorization|x-amz-signature|x-amz-credential)(\s*[:=]\s*)[^\r\n'"]*`)

// scrub removes from a provider's error message whatever it repeated of
// the request's credentials. httpx builds the message from the response
// body alone, but AWS's body can echo the signed headers back; the known
// secrets are removed too, wherever they appear whole.
func scrub(msg string, secrets ...string) string {
	msg = echoedRequest.ReplaceAllString(msg, "")
	msg = secretField.ReplaceAllString(msg, "${1}${2}[redacted]")
	for _, s := range secrets {
		if s != "" {
			msg = strings.ReplaceAll(msg, s, "[redacted]")
		}
	}
	return msg
}

// errorType is an AWS error type without its namespace or URI:
// "ValidationException:http://internal.amazon.com/coral/…" and
// "com.amazon.coral.validate#ValidationException" are both
// ValidationException.
func errorType(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndexByte(s, '#'); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// tooLong reports whether a ValidationException's message says the input
// does not fit the model, in the words Bedrock and the models behind it
// use: "Input is too long for requested model.", "prompt is too long: …",
// "input length and `max_tokens` exceed context limit", "maximum context
// length".
func tooLong(msg string) bool {
	m := strings.ToLower(msg)
	for _, s := range []string{"too long", "context", "too many input tokens", "too many tokens", "input length"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}
