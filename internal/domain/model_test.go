package domain_test

import (
	"testing"

	"github.com/lkshrk/ops-pilot/internal/domain"
)

func TestAssessmentValidCombinations(t *testing.T) {
	tests := map[string]struct {
		assessment domain.Assessment
		valid      bool
	}{
		"safe with reason and evidence": {
			assessment: domain.Assessment{Verdict: domain.AssessmentSafe, Reason: "checked manifests", Evidence: []string{"apps/api.yaml: feature absent"}}, valid: true,
		},
		"clarify with a question": {
			assessment: domain.Assessment{Verdict: domain.AssessmentClarify, Question: "Is external auth enabled?"}, valid: true,
		},
		"needs approval with an optional diff": {
			assessment: domain.Assessment{Verdict: domain.AssessmentNeedsApproval, Reason: "a manifest update is required", Diff: "--- a/app.yaml\n+++ b/app.yaml"}, valid: true,
		},
		"defer with reason": {
			assessment: domain.Assessment{Verdict: domain.AssessmentDefer, Reason: "operator chose to revisit this update later"}, valid: true,
		},
		"safe with a question": {
			assessment: domain.Assessment{Verdict: domain.AssessmentSafe, Reason: "checked", Evidence: []string{"evidence"}, Question: "Is this safe?"},
		},
		"safe with a diff": {
			assessment: domain.Assessment{Verdict: domain.AssessmentSafe, Reason: "checked", Evidence: []string{"evidence"}, Diff: "--- a/x"},
		},
		"clarify without a question": {
			assessment: domain.Assessment{Verdict: domain.AssessmentClarify},
		},
		"clarify with a diff": {
			assessment: domain.Assessment{Verdict: domain.AssessmentClarify, Question: "Which setting?", Diff: "--- a/x"},
		},
		"needs approval with a question": {
			assessment: domain.Assessment{Verdict: domain.AssessmentNeedsApproval, Reason: "risk", Question: "Which setting?"},
		},
		"safe without evidence": {
			assessment: domain.Assessment{Verdict: domain.AssessmentSafe, Reason: "checked"},
		},
		"defer without reason": {
			assessment: domain.Assessment{Verdict: domain.AssessmentDefer},
		},
		"defer with question": {
			assessment: domain.Assessment{Verdict: domain.AssessmentDefer, Reason: "later", Question: "Should I defer?"},
		},
		"defer with diff": {
			assessment: domain.Assessment{Verdict: domain.AssessmentDefer, Reason: "later", Diff: "--- a/x"},
		},
		"unknown verdict": {
			assessment: domain.Assessment{Verdict: "other", Reason: "unknown"},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if got := test.assessment.Valid(); got != test.valid {
				t.Fatalf("Valid() = %t, want %t for %+v", got, test.valid, test.assessment)
			}
		})
	}
}
