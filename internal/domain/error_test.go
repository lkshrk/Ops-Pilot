package domain_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/lkshrk/ops-pilot/internal/domain"
)

func TestErrorWrapsCause(t *testing.T) {
	cause := errors.New("merge unavailable")
	err := &domain.Error{Operation: "merge proposal", Cause: cause}
	if got, want := err.Error(), "merge proposal: merge unavailable"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(err, cause) {
		t.Fatal("Error does not unwrap its cause")
	}
}

func TestErrorWithoutCauseReturnsOperation(t *testing.T) {
	err := &domain.Error{Operation: "verify proposal"}
	if got, want := err.Error(), "verify proposal"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if errors.Unwrap(err) != nil {
		t.Fatal("Error unexpectedly unwraps a nil cause")
	}
}

func TestErrorClassOfFindsWrappedAndJoinedClass(t *testing.T) {
	configuration := &domain.Error{
		Class:     domain.ErrorConfiguration,
		Operation: "decode configuration",
		Cause:     errors.New("unknown field"),
	}
	system := &domain.Error{
		Class:     domain.ErrorSystem,
		Operation: "write report",
		Cause:     errors.New("broken pipe"),
	}

	if got := domain.ErrorClassOf(errors.Join(
		fmt.Errorf("command failed: %w", configuration),
		fmt.Errorf("cleanup failed: %w", system),
	)); got != domain.ErrorSystem {
		t.Fatalf("ErrorClassOf() = %q, want %q", got, domain.ErrorSystem)
	}
}

func TestErrorClassOfDoesNotDropUsageErrorsAsUnclassified(t *testing.T) {
	err := &domain.Error{
		Class:     domain.ErrorUsage,
		Operation: "parse command",
		Cause:     errors.New("missing proposal number"),
	}

	if got := domain.ErrorClassOf(err); got != domain.ErrorUsage {
		t.Fatalf("ErrorClassOf() = %q, want %q", got, domain.ErrorUsage)
	}
}

func TestErrorClassOfReturnsEmptyForUnclassifiedError(t *testing.T) {
	if got := domain.ErrorClassOf(errors.New("plain error")); got != "" {
		t.Fatalf("ErrorClassOf() = %q, want empty", got)
	}
}

func TestErrorClassValuesAreStable(t *testing.T) {
	got := []domain.ErrorClass{
		domain.ErrorUsage,
		domain.ErrorConfiguration,
		domain.ErrorPrerequisite,
		domain.ErrorSystem,
	}
	want := []domain.ErrorClass{"usage", "configuration", "prerequisite", "system"}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("class %d = %q, want %q", index, got[index], want[index])
		}
	}
}
