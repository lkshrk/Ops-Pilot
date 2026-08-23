// Package ai is the judgement boundary. Everything mechanical happens in Go;
// the two questions that need reading comprehension happen here.
package ai

import (
	"context"

	"github.com/lkshrk/ops-pilot/internal/domain"
)

// AssessmentRequest asks whether a dependency bump is safe to merge.
type AssessmentRequest struct {
	PullRequest    domain.PullRequest
	Dependency     domain.Dependency
	Changelog      domain.Changelog
	ChangedFiles   []string
	Clarifications []Clarification
	// Stream receives the visible parts of each assistant completion. It is nil
	// outside interactive assessments.
	Stream func(StreamEvent)
}

type StreamEventKind string

const (
	StreamTurnStart StreamEventKind = "turn_start"
	StreamDelta     StreamEventKind = "delta"
	StreamTurnEnd   StreamEventKind = "turn_end"
)

// StreamEvent describes one visible assistant completion event.
type StreamEvent struct {
	Kind StreamEventKind
	Text string
}

// Clarification is one bounded exchange from a previous assessment. It stays
// in memory for reassessment and is deliberately not a general chat history.
type Clarification struct {
	// Assistant is the complete preceding assistant turn, not just the
	// structured clarification question.
	Assistant string
	// Question remains while callers migrate to Assistant.
	Question string
	Answer   string
}

// DiagnosisRequest asks what to do about a reconcile that failed or stalled.
type DiagnosisRequest struct {
	PullRequest domain.PullRequest
	Dependency  domain.Dependency
	Failures    []domain.ObjectHealth
	Stalled     bool
	// BenignWaitUsed forbids another benign_wait: the agent must commit to a
	// fix or declare the situation unfixable.
	BenignWaitUsed bool
	FixAttempts    int
	PriorFixes     []string
	// RejectedFix is why the previous diff could not be applied, so the agent
	// can correct it rather than repeat it.
	RejectedFix string
}

// Activity reports what the agent is doing while it works. An assessment runs
// a tool-calling loop for up to a minute, which is otherwise silent.
type Activity func(tool string, argument string)

type Agent interface {
	Assess(ctx context.Context, request AssessmentRequest) (domain.Assessment, error)
	Diagnose(ctx context.Context, request DiagnosisRequest) (domain.Diagnosis, error)
}

// Tools is everything the agent may reach for. Repository reads are served from
// the local checkout at the pull request head; the rest is network.
type Tools interface {
	ReadRepoFile(ctx context.Context, path string) (string, error)
	ListRepoFiles(ctx context.Context, pattern string) ([]string, error)
	ReadUpstreamFile(ctx context.Context, repository, path, ref string) (string, error)
	SearchRepositories(ctx context.Context, query string) ([]string, error)
	Releases(ctx context.Context, repository string) (string, error)
	Issues(ctx context.Context, repository, query string) ([]string, error)
	FetchURL(ctx context.Context, url string) (string, error)
	KubePods(ctx context.Context, namespace string) (string, error)
	KubeEvents(ctx context.Context, namespace, name string) (string, error)
	KubeLogs(ctx context.Context, namespace, pod, container string) (string, error)
	FluxStatus(ctx context.Context, kind, namespace, name string) (string, error)
}
