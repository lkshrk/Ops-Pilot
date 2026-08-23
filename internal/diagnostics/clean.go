package diagnostics

import "strings"

// Clean removes both kinds of secret from untrusted text: the credential shapes
// ops-pilot never held, and the configured values it did. It normalises once,
// then collects the shape spans and the configured-value spans against that same
// normalised text and redacts their union in a single pass.
//
// The union is what no ordering of the two passes can achieve. Redacting the
// configured values first blinds the shape rules to a value whose key is itself a
// configured value; scrubbing shapes first strands the tail of a configured value
// whose head is credential-shaped; and matching either against un-normalised text
// lets an invisible rune heal a split the other pass declined. Collecting both
// span sets over one normalisation and taking their union closes all three at
// once, at the cost of over-redacting a configured value that is an ordinary word
// beside a shaped one — the safe direction.
func Clean(value string, r *Redactor) string {
	if value == "" {
		return value
	}
	n := normaliseForScrub(value)
	if n == "" {
		return n
	}
	mask := scrubShapeMask(n)
	r.markSecrets(n, mask)
	return applyMask(n, mask)
}

// markSecrets ORs each configured value's spans into mask, matching leftmost and
// longest-first exactly as the replacer would replace them, so a value that is a
// prefix of another cannot pre-empt the longer one.
func (r *Redactor) markSecrets(n string, mask []bool) {
	if r == nil || len(r.secrets) == 0 {
		return
	}
	for i := 0; i < len(n); {
		matched := 0
		for _, secret := range r.secrets {
			if secret != "" && strings.HasPrefix(n[i:], secret) {
				matched = len(secret)
				break
			}
		}
		if matched == 0 {
			i++
			continue
		}
		for k := i; k < i+matched; k++ {
			mask[k] = true
		}
		i += matched
	}
}

func applyMask(n string, mask []bool) string {
	var b strings.Builder
	b.Grow(len(n))
	for i := 0; i < len(n); {
		if !mask[i] {
			b.WriteByte(n[i])
			i++
			continue
		}
		b.WriteString(redactedValue)
		for i < len(n) && mask[i] {
			i++
		}
	}
	return b.String()
}
