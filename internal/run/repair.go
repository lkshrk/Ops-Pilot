package run

import (
	"context"
	"fmt"
	"strings"

	"github.com/lkshrk/ops-pilot/internal/adapters/github"
	"github.com/lkshrk/ops-pilot/internal/ai"
	"github.com/lkshrk/ops-pilot/internal/cluster"
	"github.com/lkshrk/ops-pilot/internal/diagnostics"
	"github.com/lkshrk/ops-pilot/internal/domain"
	"github.com/lkshrk/ops-pilot/internal/events"
	"github.com/lkshrk/ops-pilot/internal/patch"
)

// repair asks the agent what to do about a failed or stalled window, applies
// what the operator approves, and reverts when nothing else is left.
//
// A stall is diagnosed exactly like a failure. The agent may ask to wait once;
// after that extension expires it must commit to a fix or call it unfixable, so
// there is no separate rule for "never observed either way".
func (r *Runner) repair(
	ctx context.Context,
	candidate Candidate,
	baseline domain.HealthSnapshot,
	preMergeHead string,
	outcome cluster.Outcome,
	current *state,
) (domain.Attempt, string) {
	request := candidate.PullRequest
	var (
		waited          bool
		graced          bool
		priorFixes      []string
		appliedRevision string
		rejected        string
	)
	for {
		failures := failuresOf(outcome)
		current.attempt.Broken = failures
		r.step("%s.", describe(outcome))
		r.broken(failures)
		r.step("Diagnosing.")
		// Only what the model fences from here on speaks for this diagnosis.
		ai.TakeFenceForgery()
		diagnosis, err := r.agent.Diagnose(ctx, ai.DiagnosisRequest{
			PullRequest:    request,
			Dependency:     candidate.Update.Dependency,
			Failures:       failures,
			Stalled:        outcome.Result == domain.WatchStalled,
			BenignWaitUsed: waited,
			FixAttempts:    current.attempt.FixAttempts,
			PriorFixes:     priorFixes,
			RejectedFix:    rejected,
		})
		diagnosis.Cause, _ = ai.StripFenceIdentifiers(diagnosis.Cause, fenceEchoMarker)
		current.attempt.DiagnosisCause = diagnosis.Cause
		diagnosed := events.About(events.Diagnosed, request, candidate.Update.Dependency)
		diagnosed.Action, diagnosed.Reason = string(diagnosis.Action), diagnosis.Cause
		diagnosed.Objects = events.Objects(failures)
		r.emit(diagnosed)
		// A forged fence in the diagnosis inputs holds before any fix or revert;
		// detection only ever adds this hold, its absence grants nothing.
		if ai.TakeFenceForgery() {
			forged := fmt.Errorf("untrusted diagnosis input forged ops-pilot's data fence, " +
				"so a fix or revert would act on text written to be read as an instruction")
			current.attempt.Verdict, current.attempt.Error = domain.VerdictError, forged.Error()
			current.halt = fmt.Sprintf(
				"#%d is merged and its diagnosis quoted a forged data fence, so it was left in place: %v",
				request.Number, forged)
			r.log.Warnf("#%d diagnosis quoted a forged data fence, keeping the merge", request.Number)
			r.outcome(outcomeBad, "A forged data fence reached the diagnosis; the merge was left in place", forged.Error())
			if annotation := r.annotateLeftInPlace(ctx, request.Number, "the diagnosis quoted a forged data fence", forged); annotation != nil {
				current.attempt.Error = unannounced(current.attempt.Error, annotation)
				current.halt = unannounced(current.halt, annotation)
			}
			return current.done()
		}
		// cause, once set, is why this merge should be discarded. Every path that
		// reaches it goes through the same confirmation, so an operator is never
		// told about a revert only after it happened.
		cause := ""
		if err != nil {
			r.log.Warnf("#%d could not be diagnosed: %v", request.Number, err)
			// The agent quotes its own output back in errors, so this carries the
			// same model prose the cause does.
			failure, _ := ai.StripFenceIdentifiers(err.Error(), fenceEchoMarker)
			cause = "diagnosis failed: " + failure
		} else {
			switch diagnosis.Action {
			case domain.DiagnoseBenignWait:
				if waited {
					cause = "the agent asked to wait again after its one extension: " + diagnosis.Cause
					break
				}
				waited, current.attempt.Waited, graced = true, true, true
				r.outcome(outcomeAsk, "Waiting; the agent judged this benign", diagnosis.Cause)
				next, healthy, err := r.settle(ctx, baseline, outcome.Result, failures)
				if err != nil {
					return current.unobserved(ctx, r, request.Number,
						"Cluster unreadable after a benign wait; the merge was left in place",
						"the cluster could not be observed after the agent asked to wait", err)
				}
				if healthy {
					return r.healthy(current)
				}
				outcome = next
				current.attempt.Watch = outcome.Result
				continue

			case domain.DiagnoseFix:
				if len(r.options.FixAllowedPaths) == 0 {
					cause = "no fixes.allowedPaths are configured, so no fix may write anything"
					break
				}
				if current.attempt.FixAttempts >= r.options.MaxFixAttempts {
					cause = "fix attempts exhausted: " + diagnosis.Cause
					break
				}
				approved, err := r.approveFix(request, diagnosis)
				if err != nil {
					cause = err.Error()
					break
				}
				if !approved {
					cause = "fix declined: " + diagnosis.Cause
					break
				}
				sha, err := r.applyFix(ctx, diagnosis)
				if err != nil {
					// A diff that does not apply has changed nothing, so there is
					// nothing to undo and no reason to discard a merge the operator
					// approved a fix for. The agent is told why and gets to correct
					// it, within the same attempt budget.
					current.attempt.FixAttempts++
					// The refusal quotes the diff's own paths and hunks, so it carries
					// model prose exactly as a cause does.
					refusal, _ := ai.StripFenceIdentifiers(err.Error(), fenceEchoMarker)
					rejected = refusal
					r.log.Warnf("#%d fix could not be applied: %v", request.Number, refusal)
					r.outcome(outcomeAsk, "That fix did not apply", refusal)
					if current.attempt.FixAttempts >= r.options.MaxFixAttempts {
						cause = "no applicable fix was produced: " + refusal
						break
					}
					// The failure list was read before a diagnosis that takes minutes.
					// Asking for a second patch against breakage that has since gone
					// approves a change to production that nothing needs.
					still, readErr := r.observer.Broken(ctx, failures)
					if readErr != nil {
						// An unanswered health question is not an answer here either, so
						// the loop stops asking for patches and lets the pre-revert read
						// put the same question again; that one halts if it also fails.
						// It may also succeed, so the cause says why the repairing
						// stopped rather than asserting the cluster is unreadable.
						cause = "no further fix was attempted after a rejected one could not be re-checked: " + readErr.Error()
						r.log.Warnf("#%d could not re-read what broke after a rejected fix: %v", request.Number, readErr)
						break
					}
					if len(still) == 0 {
						r.step("Everything that broke has recovered; keeping the merge.")
						return r.healthy(current)
					}
					outcome = refreshed(outcome, still)
					continue
				}
				rejected = ""
				current.attempt.FixAttempts++
				current.attempt.Fixes = append(current.attempt.Fixes, diagnosis.Diff)
				// Masked in the model's copy alone: the revert reads its paths back
				// out of attempt.Fixes, and a rewritten one restores the wrong file.
				replayed, _ := ai.StripFenceIdentifiers(diagnosis.Diff, fenceEchoMarker)
				priorFixes = append(priorFixes, replayed)
				r.outcome(outcomeGood, fmt.Sprintf("Applied a fix as %s", shortSHA(sha)), diagnosis.Cause)
				applied := events.About(events.FixApplied, request, candidate.Update.Dependency)
				applied.SHA, applied.Reason = sha, diagnosis.Cause
				r.emit(applied)
				// The fix is a new commit Flux has not seen; the next window must
				// wait for that revision rather than re-reading the broken state.
				appliedRevision = sha
				if err := r.observer.Reconcile(ctx); err != nil {
					r.log.Warnf("#%d could not trigger reconciliation: %v", request.Number, err)
				}
				next, err := r.observer.Watch(ctx, baseline, appliedRevision)
				if err != nil {
					return current.unobserved(ctx, r, request.Number,
						"Cluster unreadable after the fix; the merge was left in place",
						"the cluster could not be observed after a fix was applied", err)
				}
				outcome, graced = next, false
				current.attempt.Watch = outcome.Result
				if outcome.Result == domain.WatchPass {
					return r.healthy(current)
				}
				continue

			default:
				cause = diagnosis.Cause
			}
		}

		// Diagnosing takes minutes, and a rollout can finish inside them. The
		// cluster is read once more so a merge is not discarded over an object
		// that has since recovered.
		broken, err := r.observer.Broken(ctx, failures)
		if err != nil {
			// An unanswered health question is not an answer. The stale list was
			// read before a diagnosis that takes minutes, so treating it as proof
			// discards a merge whose rollout may already have finished.
			unreadable := publishable(err.Error())
			current.attempt.Verdict, current.attempt.Error = domain.VerdictError, unreadable
			current.halt = fmt.Sprintf(
				"#%d is merged, its health could not be re-read, and it was left in place: %s",
				request.Number, unreadable)
			r.log.Warnf("#%d could not re-read what broke, keeping the merge: %v", request.Number, err)
			r.outcome(outcomeBad, "Health unknown; the merge was left in place", unreadable)
			if annotation := r.annotateLeftInPlace(ctx, request.Number, "the cluster's health could not be re-read", err); annotation != nil {
				current.attempt.Error = unannounced(current.attempt.Error, annotation)
				current.halt = unannounced(current.halt, annotation)
			}
			return current.done()
		}
		if len(broken) == 0 {
			r.step("Everything that broke has recovered; keeping the merge.")
			return r.healthy(current)
		}
		current.attempt.Broken = broken

		// A stalled window's objects were never held against the stability hold -
		// the window ended while they were still moving - so a single poll is the
		// whole evidence for discarding the merge. Give them the recovery window a
		// confirmed failure already had, once for each revision that is watched.
		if outcome.Result == domain.WatchStalled && !graced {
			graced = true
			r.step("Nothing was confirmed before the window ended; giving it one more.")
			next, recovered, err := r.settle(ctx, baseline, outcome.Result, broken)
			if err != nil {
				return current.unobserved(ctx, r, request.Number,
					"Cluster unreadable in the recovery window; the merge was left in place",
					"the cluster could not be observed during the recovery window", err)
			}
			if recovered {
				return r.healthy(current)
			}
			broken = failuresOf(next)
			if len(broken) == 0 {
				return r.healthy(current)
			}
			current.attempt.Broken = broken
		}

		// Masked once here: every consumer below publishes it, and the revert
		// commit message keeps it in the repository for good.
		cause = publishable(cause)

		choice, err := r.confirmRevert(candidate, cause, broken)
		if err != nil {
			// A prompt that could not be answered has obtained no consent, and the
			// destructive branch is the one that must not be reached by default.
			unanswered := publishable(err.Error())
			current.attempt.Verdict, current.attempt.Error = domain.VerdictError, unanswered
			current.halt = fmt.Sprintf(
				"#%d is merged, you could not be asked whether to revert it, and it was left in place: %s",
				request.Number, unanswered)
			r.log.Warnf("#%d revert prompt failed, keeping the merge: %v", request.Number, err)
			r.outcome(outcomeBad, "Could not ask whether to revert; the merge was left in place", unanswered)
			if annotation := r.annotateLeftInPlace(ctx, request.Number, "you could not be asked whether to revert it", err); annotation != nil {
				current.attempt.Error = unannounced(current.attempt.Error, annotation)
				current.halt = unannounced(current.halt, annotation)
			}
			return current.done()
		}
		switch choice {
		case RevertWait:
			next, healthy, err := r.settle(ctx, baseline, outcome.Result, broken)
			if err != nil {
				return current.unobserved(ctx, r, request.Number,
					"Cluster unreadable after you asked to wait; the merge was left in place",
					"the cluster could not be observed while waiting on your instruction", err)
			}
			if healthy {
				return r.healthy(current)
			}
			outcome, graced = next, true
			current.attempt.Watch = outcome.Result
			continue
		case RevertKeep:
			return r.kept(ctx, candidate, cause, broken, current)
		case RevertNow:
			return r.revert(ctx, candidate, baseline, preMergeHead, cause, broken, current)
		default:
			// The destructive branch is named, never defaulted to. An Approver
			// that answers with the zero RevertChoice - or anything else this
			// package does not define - has not consented to anything.
			err := fmt.Errorf("unrecognised revert answer %q", choice)
			current.attempt.Verdict, current.attempt.Error = domain.VerdictError, err.Error()
			current.halt = fmt.Sprintf(
				"#%d is merged, the answer about reverting it was not understood, and it was left in place: %v",
				request.Number, err)
			r.log.Warnf("#%d %v, keeping the merge", request.Number, err)
			r.outcome(outcomeBad, "The revert answer was not understood; the merge was left in place", err.Error())
			if annotation := r.annotateLeftInPlace(ctx, request.Number, "the answer about reverting it was not understood", err); annotation != nil {
				current.attempt.Error = unannounced(current.attempt.Error, annotation)
				current.halt = unannounced(current.halt, annotation)
			}
			return current.done()
		}
	}
}

// settle gives the broken objects one window to recover on their own, which is
// what a benign verdict and an operator's "wait" both ask for. It waits on those
// objects rather than starting another watch: a watch re-reads the same failure
// on its first poll and returns immediately, so the wait lasted no time at all.
func (r *Runner) settle(
	ctx context.Context,
	baseline domain.HealthSnapshot,
	previous domain.WatchResult,
	broken []domain.ObjectHealth,
) (cluster.Outcome, bool, error) {
	r.resetWaiting()
	outcome, err := r.observer.Restored(ctx, baseline, broken)
	if err == nil {
		return outcome, true, nil
	}
	if ctx.Err() != nil {
		return outcome, false, err
	}
	// Restored reports "did not recover" as an error; that is an answer, not a
	// failure to observe, and the loop diagnoses it like any other bad window.
	if outcome.Result == "" {
		return outcome, false, err
	}
	if len(outcome.Pending) == 0 {
		outcome.Pending = broken
	}
	// Restored has only one word for a non-recovery, and it is "stalled". A wait
	// does not change what the window found, so a failure that waited is still a
	// failure and is handed to the next diagnosis as one.
	if previous == domain.WatchFail {
		outcome.Result, outcome.Failures, outcome.Pending = previous, outcome.Pending, nil
	}
	return outcome, false, nil
}

// healthy ends the attempt on the cluster being well, whether it recovered by
// itself or because a fix landed.
func (r *Runner) healthy(current *state) (domain.Attempt, string) {
	// Watch is how the observation ended, not how it started; the earlier windows
	// are still visible in Broken, Waited and FixAttempts.
	current.attempt.Watch = domain.WatchPass
	current.attempt.Verdict = domain.VerdictMerged
	// Fixes holds the diffs that reached the branch; FixAttempts also counts the
	// ones that would not apply and so changed nothing.
	if len(current.attempt.Fixes) > 0 {
		current.attempt.Verdict = domain.VerdictFixed
	}
	r.outcome(outcomeGood, "Healthy", "")
	return current.done()
}

// confirmRevert asks the operator before a merge is discarded. An unattended run
// answers revert, which is the behaviour it has always had.
func (r *Runner) confirmRevert(
	candidate Candidate,
	cause string,
	broken []domain.ObjectHealth,
) (RevertChoice, error) {
	if r.approver == nil || !r.approver.Interactive() {
		return RevertNow, nil
	}
	return r.approver.ConfirmRevert(Revert{
		PullRequest: candidate.PullRequest,
		Dependency:  candidate.Update.Dependency,
		Cause:       cause,
		Broken:      broken,
		Window:      r.options.SettleTimeout,
	})
}

// kept records a merge an operator chose not to undo. The run continues, so the
// next pull request is watched against a cluster that still carries this
// breakage; the annotation is what tells a later reader why.
func (r *Runner) kept(
	ctx context.Context,
	candidate Candidate,
	cause string,
	broken []domain.ObjectHealth,
	current *state,
) (domain.Attempt, string) {
	request := candidate.PullRequest
	current.attempt.Verdict, current.attempt.Reason = domain.VerdictKept, cause
	r.outcome(outcomeAsk, "Kept on your instruction; the cluster is still unhealthy", cause)
	kept := events.About(events.Kept, request, candidate.Update.Dependency)
	kept.Reason, kept.Objects = cause, events.Objects(broken)
	r.emit(kept)

	var comment strings.Builder
	fmt.Fprintf(&comment, "ops-pilot would have reverted this update; an operator kept it.\n\n**Cause:** %s\n", cause)
	if current.attempt.MergeSHA != "" {
		fmt.Fprintf(&comment, "\nMerged as `%s`.\n", shortSHA(current.attempt.MergeSHA))
	}
	comment.WriteString("\n**Still unhealthy:**\n")
	for _, object := range broken {
		fmt.Fprintf(&comment, "- `%s` — %s\n", object.Ref, object.Reason)
	}
	r.comment(ctx, request.Number, comment.String())
	return current.done()
}

// comment posts on the pull request, which on a public repository is a world
// readable write. Every body it is given carries model-authored prose,
// the cluster's own reasons, or a diff, and all three can quote a secret the
// agent read out of a pod log - a value ops-pilot never held, so the configured
// redactor cannot match it and only shape matching can.
func (r *Runner) comment(ctx context.Context, number int, body string) error {
	body = r.redactor.Redact(diagnostics.ScrubSecrets(body))
	if err := r.forge.Comment(ctx, number, body); err != nil {
		r.log.Warnf("could not comment on #%d: %v", number, err)
		return err
	}
	return nil
}

func describe(outcome cluster.Outcome) string {
	if outcome.Result == domain.WatchStalled {
		return fmt.Sprintf("Stalled with %s still reconciling: %s",
			count(len(outcome.Pending), "object"), nameObjects(outcome.Pending))
	}
	names := make([]string, 0, len(outcome.Failures))
	for _, failure := range outcome.Failures {
		names = append(names, failure.Ref.String())
	}
	return "Broke " + strings.Join(names, ", ")
}

// refreshed replaces a window's objects with a newer reading of them, keeping
// the word that window used: a failure's objects belong in Failures, and
// failuresOf prefers that field.
func refreshed(outcome cluster.Outcome, objects []domain.ObjectHealth) cluster.Outcome {
	if outcome.Result == domain.WatchStalled {
		outcome.Pending, outcome.Failures = objects, nil
		return outcome
	}
	outcome.Failures, outcome.Pending = objects, nil
	return outcome
}

func failuresOf(outcome cluster.Outcome) []domain.ObjectHealth {
	if len(outcome.Failures) > 0 {
		return outcome.Failures
	}
	return outcome.Pending
}

func (r *Runner) approveFix(request domain.PullRequest, diagnosis domain.Diagnosis) (bool, error) {
	if r.approver == nil || !r.approver.Interactive() {
		return false, nil
	}
	return r.approver.ApproveFix(request, diagnosis)
}

// applyFix commits the agent's diff to the base branch. The diff is applied to
// the files as they are on the branch right now, and a hunk that does not match
// is refused rather than forced.
func (r *Runner) applyFix(ctx context.Context, diagnosis domain.Diagnosis) (string, error) {
	branch, err := r.forge.Branch(ctx)
	if err != nil {
		return "", err
	}
	head, err := r.forge.BranchHead(ctx, branch)
	if err != nil {
		return "", err
	}
	return r.applyFixAt(ctx, diagnosis, branch, head)
}

// applyFixAt is the shared patch core. repair passes the base branch's current
// head; pre-merge discussion passes the verified same-repository PR head.
func (r *Runner) applyFixAt(ctx context.Context, diagnosis domain.Diagnosis, branch, head string) (string, error) {
	// Refused rather than masked: rewriting the diff would deploy bytes the
	// operator did not approve, and the identifier belongs in no manifest.
	if _, carried := ai.StripFenceIdentifiers(diagnosis.Diff, fenceEchoMarker); carried {
		return "", fmt.Errorf(
			"refusing this fix: it writes ops-pilot's data fence identifier into the repository, " +
				"and reading those bytes back would be indistinguishable from a forged fence; " +
				"write the same change without the identifier in any path or line")
	}
	if pattern := unboundedFixPaths(r.options.FixAllowedPaths); pattern != "" {
		r.log.Warnf(
			"fixes.allowedPaths contains %q, so it excludes no path: this fix is bounded only by "+
				"the refusals for git metadata, the Flux bootstrap manifests and the governing files",
			pattern)
	}
	if pattern, probe := unprobeableKustomization(r.options.FixAllowedPaths); pattern != "" {
		r.log.Warnf(
			"fixes.allowedPaths admits %q but %s, so no fix to that kustomization can ever "+
				"land: whether it governs a Flux bootstrap directory is only answerable from the manifests "+
				"in its directory. Allow the directory rather than the file if such a fix should be able to land",
			pattern, missingSibling(probe))
	}
	files, err := parseFix(diagnosis.Diff)
	if err != nil {
		return "", err
	}
	// Every path is checked before the first read, not as each file is reached,
	// or a diff whose second section escapes still spends its first on a read.
	for _, file := range files {
		paths := diffPaths(file)
		if len(paths) == 0 {
			paths = []string{""}
		}
		for _, path := range paths {
			if err := r.allowedFixPath(path); err != nil {
				return "", err
			}
			if err := writablePath(path); err != nil {
				return "", err
			}
		}
		if err := notARename(file); err != nil {
			return "", err
		}
	}
	for _, file := range files {
		for _, path := range diffPaths(file) {
			if err := r.notABootstrapKustomization(ctx, path, head); err != nil {
				return "", err
			}
		}
	}
	changes := make([]github.FileChange, 0, len(files))
	for _, file := range files {
		path := patchPath(file)
		original, _, err := r.forge.FileAt(ctx, path, head)
		if err != nil {
			return "", err
		}
		updated, err := patch.Apply(original, file)
		if err != nil {
			return "", err
		}
		if updated == nil {
			changes = append(changes, github.FileChange{Path: path, Delete: true})
			continue
		}
		changes = append(changes, github.FileChange{Path: path, Contents: updated})
	}
	// Cleaned whole before firstLine cuts it: the scrub's key-name rules read the
	// newlines a truncation would drop.
	message := "fix: " + firstLine(r.redactProse(diagnosis.Cause)) + "\n\nApplied by ops-pilot after an operator approved the proposed fix."
	return r.forge.CreateCommit(ctx, branch, head, message, changes)
}

// revert restores every path the pull request touched to the branch content
// from before this merge, waits for the cluster to recover, then labels and
// annotates the pull request so future runs skip it.
//
// A revert that does not land, or that lands without restoring health, halts
// the run: the cluster is in a state no later attempt can be judged against.
func (r *Runner) revert(
	ctx context.Context,
	candidate Candidate,
	baseline domain.HealthSnapshot,
	preMergeHead string,
	cause string,
	broken []domain.ObjectHealth,
	current *state,
) (domain.Attempt, string) {
	request := candidate.PullRequest
	current.attempt.Reason = cause
	r.outcome(outcomeBad, "Reverting", cause)

	sha, err := r.revertCommit(ctx, request, preMergeHead, cause, current.attempt.Fixes)
	if err != nil {
		failure := publishable(err.Error())
		current.attempt.Verdict, current.attempt.Error = domain.VerdictError, "revert failed: "+failure
		current.halt = fmt.Sprintf("#%d could not be reverted and the cluster is still broken: %s", request.Number, failure)
		r.log.Warnf("#%d REVERT FAILED, cluster left as-is: %v", request.Number, err)
		if annotation := r.annotateRevertFailed(ctx, candidate, cause, err); annotation != nil {
			current.attempt.Error = unannounced(current.attempt.Error, annotation)
			current.halt = unannounced(current.halt, annotation)
		}
		return current.done()
	}
	current.attempt.RevertSHA = sha
	current.attempt.Verdict = domain.VerdictReverted
	reverted := events.About(events.Reverted, request, candidate.Update.Dependency)
	reverted.SHA, reverted.Reason = sha, cause
	reverted.Objects = events.Objects(broken)
	r.emit(reverted)

	if err := r.observer.Reconcile(ctx); err != nil {
		r.log.Warnf("#%d could not trigger reconciliation: %v", request.Number, err)
	}
	if _, err := r.observer.Restored(ctx, baseline, broken); err != nil {
		unrecovered := publishable(err.Error())
		current.attempt.Error = unrecovered
		current.halt = fmt.Sprintf("#%d was reverted but the cluster did not recover: %s", request.Number, unrecovered)
		r.log.Warnf("#%d reverted as %s but the cluster did not recover: %v", request.Number, shortSHA(sha), err)
	} else {
		r.outcome(outcomeBad, fmt.Sprintf("Reverted as %s; the cluster is back to its pre-merge health", shortSHA(sha)), "")
	}
	r.annotateReverted(ctx, request.Number, cause, &current.attempt)
	return current.done()
}

// revertCommit rewrites the paths this pull request changed, and the paths
// every fix applied during this attempt changed, back to their content on the
// branch immediately before this merge. Reverting to the pull request's own
// base SHA instead would silently undo every merge that landed between the two.
func (r *Runner) revertCommit(
	ctx context.Context,
	request domain.PullRequest,
	preMergeHead, cause string,
	fixes []string,
) (string, error) {
	if preMergeHead == "" {
		return "", fmt.Errorf("no pre-merge branch head was captured")
	}
	files, err := r.forge.ChangedFiles(ctx, request.Number)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", fmt.Errorf("pull request changed no files")
	}
	// A rename has to restore both ends: the new path disappears and the
	// original comes back, which its own entry alone would never do.
	paths := make([]string, 0, len(files)+1)
	for _, file := range files {
		paths = append(paths, file.Path)
		if file.PreviousPath != "" && file.PreviousPath != file.Path {
			paths = append(paths, file.PreviousPath)
		}
	}
	repaired, err := fixPaths(fixes)
	if err != nil {
		return "", err
	}
	paths = dedupe(append(paths, repaired...))
	branch, err := r.forge.Branch(ctx)
	if err != nil {
		return "", err
	}
	head, err := r.forge.BranchHead(ctx, branch)
	if err != nil {
		return "", err
	}
	changes := make([]github.FileChange, 0, len(paths))
	for _, path := range paths {
		before, existed, err := r.forge.FileAt(ctx, path, preMergeHead)
		if err != nil {
			return "", err
		}
		if !existed {
			changes = append(changes, github.FileChange{Path: path, Delete: true})
			continue
		}
		changes = append(changes, github.FileChange{Path: path, Contents: before})
	}
	message := fmt.Sprintf(
		"revert: #%d %s\n\nReverted by ops-pilot: %s",
		request.Number, r.redactProse(request.Title), firstLine(r.redactProse(cause)),
	)
	return r.forge.CreateCommit(ctx, branch, head, message, changes)
}

// fixPaths names every path the applied repairs wrote. A diff that no longer
// parses is an error rather than an empty list: the commit it produced is on
// the branch either way, and a revert that silently skips it would be reported
// as clean while leaving the repair deployed.
func fixPaths(fixes []string) ([]string, error) {
	var paths []string
	for _, fix := range fixes {
		files, err := parseFix(fix)
		if err != nil {
			return nil, fmt.Errorf("an applied fix can no longer be read, so its paths cannot be restored: %w", err)
		}
		for _, file := range files {
			paths = append(paths, diffPaths(file)...)
		}
	}
	return paths, nil
}

func dedupe(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	unique := make([]string, 0, len(paths))
	for _, path := range paths {
		if seen[path] {
			continue
		}
		seen[path] = true
		unique = append(unique, path)
	}
	return unique
}

// annotateRevertFailed says on the pull request that the merge is still
// deployed. It deliberately adds no label: the reverted label is what later
// runs read as "already handled", and a merge nobody managed to undo is the one
// thing a later run must not skip.
func (r *Runner) annotateRevertFailed(ctx context.Context, candidate Candidate, cause string, failure error) error {
	request := candidate.PullRequest
	reason := publishable(failure.Error())
	var comment strings.Builder
	fmt.Fprintf(&comment,
		"ops-pilot tried to revert this update and could not. **The merge is still deployed.**\n\n"+
			"**Why it wanted to revert:** %s\n\n**Why the revert failed:** %s\n\n"+
			"The run has stopped; this needs a person.\n",
		cause, reason)
	err := r.comment(ctx, request.Number, comment.String())
	failed := events.About(events.Failed, request, candidate.Update.Dependency)
	failed.Reason, failed.Error = "revert failed, the merge is still deployed: "+cause, reason
	r.emit(failed)
	return err
}

// annotateLeftInPlace says on the pull request what a halt leaves behind. The
// run stops, GitHub has already closed the pull request on the merge, and the
// only other record is stdout and the event stream, so an operator returning to
// the repository would see a closed pull request and no sign that the merge is
// deployed with nothing watching it. Like annotateRevertFailed it adds no
// label: the reverted label is what later runs read as "already handled".
func (r *Runner) annotateLeftInPlace(ctx context.Context, number int, what string, err error) error {
	var comment strings.Builder
	fmt.Fprintf(&comment,
		"ops-pilot stopped watching this update. **The merge is still deployed and nothing is watching it.**\n\n"+
			"**What stopped it:** %s: %s\n\n"+
			"The run has stopped; this needs a person.\n",
		publishable(what), publishable(err.Error()))
	return r.comment(ctx, number, comment.String())
}

func (r *Runner) annotateUnknownMerge(ctx context.Context, number int, preMergeHead string, merge, readBack error) error {
	var comment strings.Builder
	fmt.Fprintf(&comment,
		"ops-pilot could not tell whether this merged. **Check the base branch before acting.**\n\n"+
			"**Base before the merge attempt:** %s\n\n"+
			"**What the merge answered:** %s\n\n**Why it could not be re-read:** %s\n\n"+
			"The run has stopped; this needs a person.\n",
		shortSHA(preMergeHead), publishable(merge.Error()), publishable(readBack.Error()))
	return r.comment(ctx, number, comment.String())
}

// unobserved ends a pull request's section for a halt that leaves the merge
// deployed with nothing watching it. A halt stops the run, so the conclusion it
// prints is the last thing this section will ever say; an error path that only
// annotated the pull request left the section on stdout with no ending at all.
func (s *state) unobserved(
	ctx context.Context,
	r *Runner,
	number int,
	label, what string,
	err error,
) (domain.Attempt, string) {
	masked := publishable(err.Error())
	s.attempt.Verdict, s.attempt.Error = domain.VerdictError, masked
	s.halt = fmt.Sprintf("#%d is merged and could not be observed: %s", number, masked)
	r.log.Warnf("#%d %s, keeping the merge: %v", number, what, err)
	r.outcome(outcomeBad, label, masked)
	if annotation := r.annotateLeftInPlace(ctx, number, what, err); annotation != nil {
		s.attempt.Error = unannounced(s.attempt.Error, annotation)
		s.halt = unannounced(s.halt, annotation)
	}
	return s.done()
}

// publishable masks text no other seam has masked: the arbitrary errors a halt
// and a failed revert quote reach the pull request without passing a diagnosis.
func publishable(text string) string {
	masked, _ := ai.StripFenceIdentifiers(text, fenceEchoMarker)
	return masked
}

func unannounced(text string, err error) string {
	if err == nil {
		return text
	}
	return text + "; the pull request could not be annotated: " + publishable(err.Error())
}

// redactProse scrubs then redacts model-authored prose bound for a permanent
// record. The scrub runs first so its key-name rules keep the value beside the
// key; the redactor then catches a configured value the scrub's normalisation
// healed but no shape rule matched.
func (r *Runner) redactProse(text string) string {
	return r.redactor.Redact(diagnostics.ScrubSecrets(text))
}

func (r *Runner) annotateReverted(ctx context.Context, number int, cause string, attempt *domain.Attempt) {
	var comment strings.Builder
	fmt.Fprintf(&comment, "ops-pilot reverted this update.\n\n**Cause:** %s\n", cause)
	if attempt.MergeSHA != "" {
		fmt.Fprintf(&comment, "\nMerged as `%s`, reverted as `%s`.\n", shortSHA(attempt.MergeSHA), shortSHA(attempt.RevertSHA))
	}
	if len(attempt.Broken) > 0 {
		comment.WriteString("\n**What broke:**\n")
		for _, object := range attempt.Broken {
			fmt.Fprintf(&comment, "- `%s` — %s\n", object.Ref, object.Reason)
		}
	}
	if attempt.PreMergeSHA != "" {
		fmt.Fprintf(&comment, "\nThe branch was at `%s` before this merge.\n", shortSHA(attempt.PreMergeSHA))
	}
	for i, fix := range attempt.Fixes {
		// Masked here rather than in attempt.Fixes: the revert reads its paths back
		// out of those diffs, and a rewritten one restores the wrong file.
		published, _ := ai.StripFenceIdentifiers(fix, fenceEchoMarker)
		fmt.Fprintf(&comment, "\nFix %d, applied and not sufficient:\n\n```diff\n%s\n```\n", i+1, published)
	}
	comment.WriteString("\nRemove the `" + r.options.RevertedLabel + "` label to let ops-pilot try again.")

	r.comment(ctx, number, comment.String())
	if err := r.forge.AddLabel(ctx, number, r.options.RevertedLabel); err != nil {
		r.log.Warnf("could not label #%d: %v", number, err)
		return
	}
	r.step("Labelled #%d %s.", number, r.options.RevertedLabel)
	r.emit(events.Event{
		Kind:        events.Labelled,
		PullRequest: number,
		Label:       r.options.RevertedLabel,
		Reason:      cause,
	})
}

func firstLine(value string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(value), "\n")
	return line
}
