package ai

import "strings"

// StripFenceIdentifiers masks every fence identifier this process has issued,
// live or retired, and reports whether the text carried one. Only the exact
// identifiers are masked: a pattern would eat object references, digests and
// commit shas out of prose an operator reads.
func StripFenceIdentifiers(text, replacement string) (string, bool) {
	nonces := issuedFenceNonces()
	masked := text
	for _, nonce := range nonces {
		masked = strings.ReplaceAll(masked, nonce, replacement)
	}
	// Masking one identifier can splice its neighbours into another, and the
	// replacement itself may carry one; deleting shortens, so this terminates.
	for {
		pass := masked
		for _, nonce := range nonces {
			pass = stripNonce(pass, nonce)
		}
		if pass == masked {
			break
		}
		masked = pass
	}
	return masked, masked != text
}

// FenceMarkerWithoutIdentifier reports that text forges a fence with nothing
// this run issued, so it closes no marker the model was told to trust. Callers
// refuse text of their own making on that basis; text ops-pilot received still
// latches through FenceData, where a marker is a fact about the data.
func FenceMarkerWithoutIdentifier(text string) bool {
	return FenceForged(text) && !fenceNonceIssued(text)
}

// LatchFenceIdentifier trips the forgery record when text carries an identifier
// this run issued, live or retired, and reports whether it did. The tool seam
// calls it on the arguments the model supplied: an identifier reaches an argument
// the model authored only if fenced data steered the model into spelling it, so
// it is forgery the moment it arrives - before any tool runs and whatever the
// tool returns. FenceData latches the same identifier when a result echoes it,
// but a call the injection chose can return nothing to echo, so the latch cannot
// wait for the result.
func LatchFenceIdentifier(text string) bool {
	if fenceNonceIssued(text) {
		fenceForgery.Store(true)
		return true
	}
	return false
}

func issuedFenceNonces() []string {
	fenceNonceMu.Lock()
	defer fenceNonceMu.Unlock()
	nonces := make([]string, 0, len(fenceNonceRetired)+1)
	if fenceNonceLive != "" {
		nonces = append(nonces, fenceNonceLive)
	}
	return append(nonces, fenceNonceRetired...)
}
