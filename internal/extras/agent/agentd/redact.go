package agentd

import (
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Redaction happens once, where the journal is written, so no downstream consumer
// can leak what it never received.

// minSecretLen keeps a registered value like "true" from matching everywhere.
const minSecretLen = 8

// credentialPatterns match well-known credential shapes for secrets nobody
// registered. Biased toward false positives.
var credentialPatterns = []*regexp.Regexp{
	// Anthropic / OpenAI, incl. sk-ant-* and sk-proj-*.
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`),
	// xAI.
	regexp.MustCompile(`xai-[A-Za-z0-9_-]{20,}`),
	// GitHub tokens and fine-grained PATs.
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{16,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
	// AWS access key id.
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	// Slack tokens.
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),
	// Google API keys.
	regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`),
	// JWTs.
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),
	// Agent access tokens.
	regexp.MustCompile(`sfa_[A-Za-z0-9]{6,}_[A-Za-z0-9]{16,}`),
	// PEM private keys, whole block.
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`),
}

const patternMarker = "[redacted]"

// Redactor removes registered secret values and credential-shaped strings from
// text. Safe for concurrent use.
type Redactor struct {
	mu sync.RWMutex
	// secrets is name to value.
	secrets map[string]string
}

// NewRedactor returns a redactor with no registered secrets.
func NewRedactor() *Redactor { return &Redactor{secrets: map[string]string{}} }

// AddSecret registers an exact value to remove; the marker names it as
// "[redacted:NAME]". Values shorter than minSecretLen are ignored.
func (r *Redactor) AddSecret(name, value string) {
	if len(value) < minSecretLen {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.secrets == nil {
		r.secrets = map[string]string{}
	}
	r.secrets[name] = value
}

// Redact returns s with every registered secret and credential-shaped substring
// replaced. Called on every output chunk, so the no-match path must not allocate.
func (r *Redactor) Redact(s string) string {
	if s == "" {
		return s
	}
	if r != nil {
		r.mu.RLock()
		type kv struct{ name, val string }
		var list []kv
		for n, v := range r.secrets {
			if strings.Contains(s, v) {
				list = append(list, kv{n, v})
			}
		}
		r.mu.RUnlock()

		// Longest first, so a secret containing another is replaced whole.
		sort.Slice(list, func(i, j int) bool { return len(list[i].val) > len(list[j].val) })
		for _, e := range list {
			s = strings.ReplaceAll(s, e.val, "[redacted:"+e.name+"]")
		}
	}
	for _, re := range credentialPatterns {
		if re.MatchString(s) {
			s = re.ReplaceAllString(s, patternMarker)
		}
	}
	return s
}
