package diagnostics_test

import (
	"strings"
	"testing"

	"github.com/lkshrk/ops-pilot/internal/diagnostics"
)

func TestStreamRedactorHoldsSecretsAcrossEveryDeltaBoundary(t *testing.T) {
	tests := []struct {
		name   string
		secret string
		text   string
	}{
		{"configured", "configured-token-value", "model said configured-token-value "},
		{"configured spaces", "top secret value", "model said top secret value "},
		{"configured newline", "first\nsecond", "model said first\nsecond "},
		{"rendered configured", "hidden\u200bvalue", "model said hiddenvalue "},
		{"github token", "", "token=ghp_0123456789abcdefghijklmnop "},
		{"authorization", "", "authorization: Bearer abcdefghijklmnopqrstuvwxyz "},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for split := 0; split <= len(test.text); split++ {
				stream := diagnostics.NewRedactor([]string{test.secret}).Stream()
				got := stream.Write(test.text[:split]) + stream.Write(test.text[split:]) + stream.Flush()
				if test.secret != "" && strings.Contains(got, test.secret) {
					t.Fatalf("split %d leaked configured secret: %q", split, got)
				}
				if test.name == "rendered configured" && strings.Contains(got, "hiddenvalue") {
					t.Fatalf("split %d leaked rendered configured secret: %q", split, got)
				}
				if strings.Contains(got, "ghp_0123456789abcdefghijklmnop") || strings.Contains(got, "abcdefghijklmnopqrstuvwxyz") {
					t.Fatalf("split %d leaked credential shape: %q", split, got)
				}
				if !strings.Contains(got, "[REDACTED]") {
					t.Fatalf("split %d was not redacted: %q", split, got)
				}
			}
		})
	}
}

func TestStreamRedactorStillEmitsOrdinaryCompleteWords(t *testing.T) {
	stream := diagnostics.NewRedactor(nil).Stream()
	if got := stream.Write("ordinary text "); got != "ordinary text " {
		t.Fatalf("first ordinary segment = %q", got)
	}
	if got := stream.Write("keeps arriving "); got != "keeps arriving " {
		t.Fatalf("second ordinary segment = %q", got)
	}
}

func TestStreamRedactorResumesAfterAScrubbedCredentialLine(t *testing.T) {
	stream := diagnostics.NewRedactor(nil).Stream()
	const token = "ghp_0123456789abcdefghijklmnop"
	if got := stream.Write("token: " + token + "\n"); strings.Contains(got, token) || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("credential line = %q", got)
	}
	if got := stream.Write("ordinary later segments "); got != "ordinary later segments " {
		t.Fatalf("later prose was buffered until turn end: %q", got)
	}
}
