package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/lkshrk/ops-pilot/internal/diagnostics"
	"github.com/lkshrk/ops-pilot/internal/domain"
)

// cleaned and structural are the hand-made decisions the guards below check.
// record names the fields it cleans one by one and nothing connects that list to
// the type, so declaring a field here is a claim about what reaches the store:
// one guard insists a decision exists, the other insists it is carried out.
var (
	cleaned = map[string]bool{
		"Attempt.Reason": true, "Attempt.DiagnosisCause": true, "Attempt.Error": true,
		"Attempt.Evidence": true, "Attempt.Fixes": true, "ObjectHealth.Reason": true,
	}
	// Set on the run and not on an attempt, so the recorder guard below cannot
	// plant it; the run guard drives a halting run instead.
	runCleaned = map[string]bool{
		"Run.Halted": true,
	}
	structural = map[string]bool{
		"Attempt.RunID": true, "Attempt.Decision": true, "Attempt.Verdict": true,
		"Attempt.ChangelogSource": true, "Attempt.ChangelogURL": true,
		"Attempt.HeadSHA": true, "Attempt.PreMergeSHA": true, "Attempt.MergeSHA": true,
		"Attempt.RevertSHA": true, "Attempt.Watch": true,
		"Dependency.Name": true, "Dependency.Kind": true,
		"Dependency.FromVersion": true, "Dependency.ToVersion": true, "Dependency.Bump": true,
		"ObjectHealth.Revision": true,
		"ObjectRef.Kind":        true, "ObjectRef.Namespace": true, "ObjectRef.Name": true,
		"Run.ID": true, "Run.Mode": true,
		"RepositoryRef.Owner": true, "RepositoryRef.Name": true, "RepositoryRef.Branch": true,
	}
)

// The attempt outlives the run in SQLite, and its prose quotes pod logs and
// repository files. A field added later is cleaned or exempt by a decision
// someone made, not by being forgotten. The run row outlives it the same way, so
// it is walked too: its halt reason is built from the same untrusted text.
func TestEveryPersistedStringFieldIsEitherCleanedOrDeclaredStructural(t *testing.T) {
	seen := map[reflect.Type]bool{}
	reached := map[string]bool{}
	var walk func(shape reflect.Type, name string)
	walk = func(shape reflect.Type, name string) {
		switch shape.Kind() {
		case reflect.String:
			reached[name] = true
			if !cleaned[name] && !runCleaned[name] && !structural[name] {
				t.Errorf("%s is neither cleaned nor declared structural", name)
			}
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(shape.Elem(), name)
		case reflect.Map:
			walk(shape.Key(), name+" key")
			walk(shape.Elem(), name)
		case reflect.Interface:
			t.Errorf("%s is an interface, so no decision about its contents can be made here", name)
		case reflect.Struct:
			if seen[shape] {
				return
			}
			seen[shape] = true
			owner := shape.Name()
			if owner == "" {
				owner = name
			}
			for i := range shape.NumField() {
				if field := shape.Field(i); field.IsExported() {
					walk(field.Type, owner+"."+field.Name)
				}
			}
		}
	}
	walk(reflect.TypeOf(domain.Attempt{}), "Attempt")
	walk(reflect.TypeOf(domain.Run{}), "Run")

	// Without this the guard passes on a root that reaches nothing, and every
	// declaration below becomes a claim about a type no longer being walked.
	for _, declarations := range []map[string]bool{cleaned, runCleaned, structural} {
		for name := range declarations {
			if !reached[name] {
				t.Errorf("%s is declared here but the walk never reaches it", name)
			}
		}
	}
	for name := range cleaned {
		if structural[name] || runCleaned[name] {
			t.Errorf("%s is declared both cleaned and structural", name)
		}
	}
	for name := range runCleaned {
		if structural[name] {
			t.Errorf("%s is declared both cleaned and structural", name)
		}
	}
}

// Declaring a field cleaned is otherwise a green no-op, so the assertion is
// driven off the map: every name in it is planted with a secret and the attempt
// handed to the recorder is read back.
func TestEveryFieldDeclaredCleanedIsCleanedBeforeItIsRecorded(t *testing.T) {
	const (
		configured = "a-configured-value"
		shaped     = "Ai8fkq2LmZx0Rt7Yb3Nc"
	)

	for name := range cleaned {
		t.Run(name, func(t *testing.T) {
			attempt := domain.Attempt{}
			text := "quoted api_key=" + shaped + " using " + configured
			if !plant(reflect.ValueOf(&attempt).Elem(), "Attempt", name, text) {
				t.Fatalf("%s is declared cleaned but no such field exists on the type", name)
			}

			recorder := &recordingRecorder{}
			runner := New(Dependencies{
				Recorder: recorder,
				Redactor: diagnostics.NewRedactor([]string{configured}),
			}, Options{})
			runner.record(context.Background(), attempt)

			if len(recorder.attempts) != 1 {
				t.Fatalf("want one recorded attempt, got %d", len(recorder.attempts))
			}
			raw, err := json.Marshal(recorder.attempts[0])
			if err != nil {
				t.Fatalf("marshal the recorded attempt: %v", err)
			}
			stored := string(raw)
			for _, secret := range []string{shaped, configured} {
				if strings.Contains(stored, secret) {
					t.Fatalf("%s is declared cleaned and reached the store with %q: %s", name, secret, stored)
				}
			}
			if !strings.Contains(stored, "[REDACTED]") {
				t.Fatalf("%s vanished instead of being redacted: %s", name, stored)
			}
		})
	}
}

// Declaring a field runCleaned is a green no-op for the same reason, and it has
// no recorder to plant through: the run row is written by Run itself. So the
// driver is a run that halts on untrusted text, and every name in the map is
// read back off the run it returns. A name the halt path never fills is a claim
// about nothing, which is the state this guard was added to end.
func TestEveryFieldDeclaredRunCleanedIsCleanedBeforeItIsStored(t *testing.T) {
	for name := range runCleaned {
		t.Run(name, func(t *testing.T) {
			g := newGate(t)
			recorder := &recordingRecorder{}
			g.runner.recorder = recorder
			g.runner.options.OnlyPullRequest = 1204
			request := g.forge.requests[1204]
			request.Body = renovateBody("sonarr", "4.0.14", "4.0.19")
			g.forge.requests[1204] = request
			g.runner.observer = &unobservableCluster{
				gateObserver: g.observer,
				err: fmt.Errorf("flux said api_key=%s using %s",
					workloadSecret, configuredSecret),
			}

			finished, err := g.runner.Run(context.Background())
			if !errors.Is(err, ErrHalted) {
				t.Fatalf("run: %v, want a halt", err)
			}
			if recorder.halted == "" {
				t.Fatal("the run did not halt; the fixture no longer reaches the halt path")
			}

			planted := harvest(reflect.ValueOf(finished), "Run", name)
			if len(planted) == 0 {
				t.Fatalf("%s is declared cleaned but the halting run never fills it", name)
			}
			for _, value := range planted {
				for _, secret := range []string{workloadSecret, configuredSecret} {
					if strings.Contains(value, secret) {
						t.Errorf("%s is declared cleaned and kept %q: %q", name, secret, value)
					}
				}
				if !strings.Contains(value, "[REDACTED]") {
					t.Errorf("%s carries no redaction, so nothing untrusted reached it: %q", name, value)
				}
			}
		})
	}
}

// harvest reads back every string the walk would call target. It walks the way
// the structural guard walks, so a shape one of them can reach and the other
// cannot is a bug in this file.
func harvest(value reflect.Value, name, target string) []string {
	switch value.Kind() {
	case reflect.String:
		if name != target || value.String() == "" {
			return nil
		}
		return []string{value.String()}
	case reflect.Pointer, reflect.Interface:
		if value.IsNil() {
			return nil
		}
		return harvest(value.Elem(), name, target)
	case reflect.Slice, reflect.Array:
		var found []string
		for i := range value.Len() {
			found = append(found, harvest(value.Index(i), name, target)...)
		}
		return found
	case reflect.Map:
		var found []string
		for iter := value.MapRange(); iter.Next(); {
			found = append(found, harvest(iter.Key(), name+" key", target)...)
			found = append(found, harvest(iter.Value(), name, target)...)
		}
		return found
	case reflect.Struct:
		owner := value.Type().Name()
		if owner == "" {
			owner = name
		}
		var found []string
		for i := range value.NumField() {
			if field := value.Type().Field(i); field.IsExported() {
				found = append(found, harvest(value.Field(i), owner+"."+field.Name, target)...)
			}
		}
		return found
	}
	return nil
}

// plant writes text into the field named target, allocating whatever containers
// stand between it and the root. It walks the way the structural guard walks, so
// a shape one of them can reach and the other cannot is a bug in this file.
func plant(value reflect.Value, name, target, text string) bool {
	switch value.Kind() {
	case reflect.String:
		if name != target {
			return false
		}
		value.SetString(text)
		return true
	case reflect.Pointer:
		value.Set(reflect.New(value.Type().Elem()))
		return plant(value.Elem(), name, target, text)
	case reflect.Slice:
		value.Set(reflect.MakeSlice(value.Type(), 1, 1))
		if plant(value.Index(0), name, target, text) {
			return true
		}
		value.Set(reflect.Zero(value.Type()))
		return false
	case reflect.Array:
		if value.Len() == 0 {
			return false
		}
		return plant(value.Index(0), name, target, text)
	case reflect.Map:
		entry := reflect.New(value.Type().Elem()).Elem()
		if !plant(entry, name, target, text) {
			return false
		}
		key := reflect.New(value.Type().Key()).Elem()
		value.Set(reflect.MakeMap(value.Type()))
		value.SetMapIndex(key, entry)
		return true
	case reflect.Struct:
		owner := value.Type().Name()
		if owner == "" {
			owner = name
		}
		for i := range value.NumField() {
			if !value.Type().Field(i).IsExported() {
				continue
			}
			field := owner + "." + value.Type().Field(i).Name
			if plant(value.Field(i), field, target, text) {
				return true
			}
		}
	}
	return false
}
