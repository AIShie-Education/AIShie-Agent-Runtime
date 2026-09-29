package vault

import (
	"strings"
	"unicode/utf8"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// Ellipsis stands for what a hint leaves out.
const Ellipsis = "…"

// coreTokenPrefix begins Core's API tokens: ais_, then a public prefix of
// 12 characters, then _ and the secret. Core lists a token by that prefix
// (credential_list's token_prefix), so a hint that shows it names the
// token in Core and nothing of its secret.
const (
	coreTokenPrefix = "ais_"
	corePublicLen   = 12
)

// keyPrefixes are providers' key prefixes that say whose a key is and
// nothing of it, longest first where one begins another.
var keyPrefixes = []string{
	"sk-ant-", "sk-proj-", "sk-svcacct-", "sk-admin-", "sk-or-",
	"sk-", "AIza", "ABSK",
}

// A hint shows a key's last four characters only when the key has at least
// minTailedKey characters, and minTailedBody after its prefix: of a
// shorter one, four would be too much of it.
const (
	minTailedKey  = 20
	minTailedBody = 16
)

// Hint is what may be shown of a secret of kind, and never more: for a
// Core token, ais_ and its public prefix (ais_k7v2m4qhx3ab…), nothing for
// a token not of Core's shape; for a model
// key, the provider's prefix and the last four characters (sk-…3f9a), the
// prefix alone for a key too short to show four of, and the last four
// alone for a key of no known prefix.
func Hint(kind, secret string) string {
	secret = strings.TrimSpace(secret)
	if kind == store.SecretCoreToken {
		// The prefix is the 12 characters between ais_ and the next _;
		// twelve characters with no _ after them may be the secret's.
		public, ok := strings.CutPrefix(secret, coreTokenPrefix)
		if !ok || len(public) <= corePublicLen || public[corePublicLen] != '_' || !isPublicPrefix(public[:corePublicLen]) {
			return Ellipsis
		}
		return coreTokenPrefix + public[:corePublicLen] + Ellipsis
	}
	prefix := ""
	for _, p := range keyPrefixes {
		if strings.HasPrefix(secret, p) {
			prefix = p
			break
		}
	}
	n := utf8.RuneCountInString(secret)
	if n < minTailedKey || n-utf8.RuneCountInString(prefix) < minTailedBody {
		return prefix + Ellipsis
	}
	r := []rune(secret)
	return prefix + Ellipsis + string(r[len(r)-4:])
}

// isPublicPrefix reports whether s is a token's public prefix as Core makes
// them: lower-case letters and the digits 2 to 7.
func isPublicPrefix(s string) bool {
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			return false
		}
	}
	return true
}
