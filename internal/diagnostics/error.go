// Package diagnostics provides value-redacted logging, error rendering, and
// startup prerequisite checks.
package diagnostics

import (
	"regexp"
	"sort"
	"strings"
)

const redactedValue = "[REDACTED]"

// Redactor replaces the non-empty secret values supplied at construction, each
// in both the form configured and the form its sinks render. Its state is
// immutable and safe for concurrent use.
type Redactor struct {
	replacer *strings.Replacer
	// secrets holds the same values as replacer, longest first, so Clean can
	// collect their spans over normalised text the way replacer replaces them.
	secrets []string
}

// NewRedactor copies, deduplicates, and pins the supplied secret values.
func NewRedactor(values []string) *Redactor {
	unique := make(map[string]struct{}, len(values)*2)
	for _, value := range values {
		if value == "" {
			continue
		}
		unique[value] = struct{}{}
		// A sink that scrubs before it redacts hands over the value with its
		// invisible runes already dropped, so that form is a secret here too -
		// but only above the shape rules' {4,} floor, or a rendered form that
		// collapses to a common word nukes it out of unrelated prose.
		if rendered := Storable(value); len(rendered) >= 4 {
			unique[rendered] = struct{}{}
		}
	}
	secrets := make([]string, 0, len(unique))
	for secret := range unique {
		secrets = append(secrets, secret)
	}
	sort.Slice(secrets, func(i, j int) bool {
		if len(secrets[i]) != len(secrets[j]) {
			return len(secrets[i]) > len(secrets[j])
		}
		return secrets[i] < secrets[j]
	})
	pairs := make([]string, 0, len(secrets)*2)
	for _, secret := range secrets {
		pairs = append(pairs, secret, redactedValue)
	}
	redactor := &Redactor{secrets: secrets}
	if len(pairs) > 0 {
		redactor.replacer = strings.NewReplacer(pairs...)
	}
	return redactor
}

// RenderError renders an error with both the configured values and the
// credential shapes removed from its complete wrapped or joined message.
func RenderError(err error, redactor *Redactor) string {
	if err == nil {
		return ""
	}
	// This wraps stderr from git, GitHub and container registries, so it is
	// untrusted text on the one line an operator always reads. The shape pass
	// runs first: terminalSafe deletes the newlines the key-name rules read.
	return terminalSafe(redactor.Redact(ScrubSecrets(err.Error())))
}

// Redact replaces the configured values and nothing else. It cannot see a
// credential ops-pilot never held, which is what model prose quoting a pod log
// carries; ScrubSecrets is the half that matches those, and a sink that prints
// untrusted text needs both.
func (r *Redactor) Redact(value string) string {
	if r == nil || r.replacer == nil || value == "" {
		return value
	}
	return r.replacer.Replace(value)
}

// Stream returns a redactor for progressively displayed untrusted prose. It
// withholds only text which can still complete a configured secret, and keeps
// a credential-looking line until ScrubSecrets can see its complete value.
func (r *Redactor) Stream() *StreamRedactor { return &StreamRedactor{redactor: r} }

// StreamRedactor is intentionally per stream: its tails make values split by
// model deltas indistinguishable from values delivered in one piece.
type StreamRedactor struct {
	redactor   *Redactor
	configured string
	shape      string
	sensitive  bool
}

var streamCredentialName = regexp.MustCompile(`(?i)(` + credentialNames + `|authorization|(?:set-)?cookie|tls\.key|private key|--(?:pass(?:word|wd|phrase)?|token|api-?key|secret)[ =])`)

// Write returns the longest prefix now safe to display. It preserves ordinary
// word-level progress while never emitting a configured secret or an
// incomplete credential-shaped token.
func (s *StreamRedactor) Write(text string) string {
	if text == "" {
		return ""
	}
	s.configured += text
	return s.scrub(s.takeConfigured(false), false)
}

// Flush returns the final safe text when the model turn ends.
func (s *StreamRedactor) Flush() string { return s.scrub(s.takeConfigured(true), true) }

func (s *StreamRedactor) takeConfigured(final bool) string {
	if final {
		value := s.configured
		s.configured = ""
		return s.redactor.Redact(value)
	}
	hold := 0
	if s.redactor != nil {
		for _, secret := range s.redactor.secrets {
			limit := len(secret) - 1 // A complete secret is safe to replace now.
			if limit > len(s.configured) {
				limit = len(s.configured)
			}
			for n := limit; n > hold; n-- {
				if strings.HasPrefix(secret, s.configured[len(s.configured)-n:]) {
					hold = n
					break
				}
			}
		}
	}
	value := s.configured[:len(s.configured)-hold]
	s.configured = s.configured[len(s.configured)-hold:]
	return s.redactor.Redact(value)
}

func (s *StreamRedactor) scrub(value string, final bool) string {
	s.shape += value
	if final {
		value = ScrubSecrets(s.shape)
		s.shape = ""
		s.sensitive = false
		return value
	}
	var out strings.Builder
	for {
		if !s.sensitive && streamCredentialName.MatchString(s.shape) {
			s.sensitive = true
		}
		if s.sensitive {
			line := strings.IndexByte(s.shape, '\n')
			if line < 0 {
				return out.String()
			}
			out.WriteString(ScrubSecrets(s.shape[:line+1]))
			s.shape = s.shape[line+1:]
			s.sensitive = false
			continue
		}
		// Keep the final whitespace-delimited token: the shape scrubber needs its
		// terminator before it can decide whether a credential-shaped span is real.
		cut := strings.LastIndexAny(s.shape, " \t\r\n")
		if cut < 0 {
			return out.String()
		}
		cut++
		out.WriteString(ScrubSecrets(s.shape[:cut]))
		s.shape = s.shape[cut:]
		return out.String()
	}
}
