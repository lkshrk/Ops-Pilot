package events_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/lkshrk/ops-pilot/internal/diagnostics"
	"github.com/lkshrk/ops-pilot/internal/domain"
	"github.com/lkshrk/ops-pilot/internal/events"
)

func open(t *testing.T) (*events.Emitter, string) {
	t.Helper()
	return openWith(t, nil)
}

func openWith(t *testing.T, redactor *diagnostics.Redactor) (*events.Emitter, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "run.jsonl")
	moment := time.Date(2026, 8, 2, 14, 0, 0, 0, time.UTC)
	emitter, err := events.Open(path, func() time.Time { return moment }, redactor)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = emitter.Close() })
	emitter.Bind(domain.Run{ID: "run-1", Repository: domain.RepositoryRef{Owner: "a", Name: "b"}})
	return emitter, path
}

func lines(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var decoded []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("line is not JSON: %q: %v", line, err)
		}
		decoded = append(decoded, event)
	}
	return decoded
}

func TestEveryEventCarriesTheRunItBelongsTo(t *testing.T) {
	emitter, path := open(t)
	emitter.Emit(events.Event{Kind: events.RunStarted})
	emitter.Emit(events.Event{Kind: events.Merged, PullRequest: 714, SHA: "abc1234"})

	for _, event := range lines(t, path) {
		if event["runId"] != "run-1" || event["repository"] != "a/b" {
			t.Fatalf("event is not attributable: %v", event)
		}
		if event["ts"] != "2026-08-02T14:00:00Z" {
			t.Fatalf("event has no usable timestamp: %v", event)
		}
	}
}

// A consumer matches on presence, so an absent field must not appear empty.
func TestAbsentFieldsAreOmitted(t *testing.T) {
	emitter, path := open(t)
	emitter.Emit(events.Event{Kind: events.RunStarted})

	event := lines(t, path)[0]
	for _, absent := range []string{"pr", "sha", "objects", "reason", "error"} {
		if _, present := event[absent]; present {
			t.Fatalf("%q should be omitted when unset: %v", absent, event)
		}
	}
}

// An alert should be able to name the affected service, not just the PR.
func TestBrokenObjectsCarryTheirReasons(t *testing.T) {
	emitter, path := open(t)
	emitter.Emit(events.Event{
		Kind: events.Reverted,
		Objects: events.Objects([]domain.ObjectHealth{{
			Ref:    domain.ObjectRef{Kind: "HelmRelease", Namespace: "media", Name: "sonarr"},
			Reason: "upgrade failed",
		}}),
	})

	objects, ok := lines(t, path)[0]["objects"].([]any)
	if !ok || len(objects) != 1 {
		t.Fatalf("want one object: %v", lines(t, path)[0])
	}
	object := objects[0].(map[string]any)
	if object["ref"] != "media/HelmRelease/sonarr" || object["reason"] != "upgrade failed" {
		t.Fatalf("object lost its detail: %v", object)
	}
}

// The stream observes a run that is changing a cluster; it must never stop one.
func TestAWriteFailureDoesNotPanicOrBlock(t *testing.T) {
	emitter, _ := open(t)
	if err := emitter.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	emitter.Emit(events.Event{Kind: events.Merged, PullRequest: 1})
	emitter.Emit(events.Event{Kind: events.Merged, PullRequest: 2})

	if emitter.Err() == nil {
		t.Fatal("a write failure should be reported once")
	}
}

// The stream is piped to a file or a log shipper, so it outlives the terminal
// and lands somewhere searchable; a secret quoted in it is durable.
func TestProseFieldsAreScrubbedBeforeTheyReachTheStream(t *testing.T) {
	const (
		jwt   = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N-XgL0n3I9PlFUP0THsR8U"
		dbURL = "postgres://app:hunter2swordfish@db.internal:5432/prod"
		named = "api_key=Ai8fkq2LmZx0Rt7Yb3Nc"
		basic = "Authorization: Basic aGVsbG86c3VwZXJzZWNyZXR0b2tlbg=="
	)
	tests := []struct {
		name   string
		event  events.Event
		secret string
		field  func(map[string]any) any
	}{
		{
			name:   "a diagnosis cause quoting a pod log",
			event:  events.Event{Kind: events.Diagnosed, Reason: "the pod logs " + jwt + " on startup"},
			secret: jwt,
			field:  func(event map[string]any) any { return event["reason"] },
		},
		{
			name:   "a wrapped error echoing a credentialed url",
			event:  events.Event{Kind: events.Failed, Error: "clone failed: " + dbURL},
			secret: "hunter2swordfish",
			field:  func(event map[string]any) any { return event["error"] },
		},
		{
			name:   "a halt reason echoing a key-named value",
			event:  events.Event{Kind: events.Halted, Reason: "config rejected: " + named},
			secret: "Ai8fkq2LmZx0Rt7Yb3Nc",
			field:  func(event map[string]any) any { return event["reason"] },
		},
		{
			name: "a controller message on a broken object",
			event: events.Event{Kind: events.Reverted, Objects: events.Objects([]domain.ObjectHealth{{
				Ref:    domain.ObjectRef{Kind: "HelmRelease", Namespace: "media", Name: "sonarr"},
				Reason: "upgrade failed: " + basic,
			}})},
			secret: "aGVsbG86c3VwZXJzZWNyZXR0b2tlbg==",
			field: func(event map[string]any) any {
				return event["objects"].([]any)[0].(map[string]any)["reason"]
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			emitter, path := open(t)
			emitter.Emit(test.event)

			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if strings.Contains(string(raw), test.secret) {
				t.Fatalf("the secret reached the stream: %s", raw)
			}
			value, _ := test.field(lines(t, path)[0]).(string)
			if !strings.Contains(value, "[REDACTED]") {
				t.Fatalf("want the field to say what was removed, got %q", value)
			}
		})
	}
}

// The stream has shapes but no configured values, while stdout and the history
// database have configured values but no shapes. A registry password that is an
// ordinary word carries no shape at all and is caught by nothing else.
func TestAConfiguredSecretWithNoShapeIsRedactedFromTheStream(t *testing.T) {
	const password = "correcthorsebatterystaple"
	tests := []struct {
		name  string
		event events.Event
	}{
		{
			name:  "a diagnosis quoting the configured registry password",
			event: events.Event{Kind: events.Diagnosed, Reason: "the pull used " + password},
		},
		{
			name:  "an error quoting it",
			event: events.Event{Kind: events.Failed, Error: "login failed for " + password},
		},
		{
			name: "a controller message on a broken object quoting it",
			event: events.Event{Kind: events.Reverted, Objects: events.Objects([]domain.ObjectHealth{{
				Ref:    domain.ObjectRef{Kind: "HelmRelease", Namespace: "media", Name: "sonarr"},
				Reason: "pull failed: " + password,
			}})},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			emitter, path := openWith(t, diagnostics.NewRedactor([]string{password}))
			emitter.Emit(test.event)

			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if strings.Contains(string(raw), password) {
				t.Fatalf("the configured secret reached the stream: %s", raw)
			}
		})
	}
}

// Held as code points, not literals: a literal carrier is invisible to review
// and staticcheck rejects several of them outright.
var invisibleCarriers = []struct {
	name    string
	carrier rune
}{
	{name: "zero width space", carrier: 0x200B},
	{name: "zero width non joiner", carrier: 0x200C},
	{name: "zero width joiner", carrier: 0x200D},
	{name: "word joiner", carrier: 0x2060},
	{name: "byte order mark", carrier: 0xFEFF},
	{name: "soft hyphen", carrier: 0x00AD},
	{name: "right to left override", carrier: 0x202E},
	{name: "private use", carrier: 0xE000},
	{name: "carriage return", carrier: 0x000D},
}

func splitBy(carrier rune, head, tail string) string {
	return head + string(carrier) + tail
}

// Normalising the text before the needles run closes C-M89's split, but it lets
// a configured value that happens to be a key token eat the key, after which the
// shape rules no longer recognise "[REDACTED]: value" as a key-value pair and the
// workload's own credential reaches `jq -r` whole. Neither pass may destroy the
// evidence the other matches on.
func TestRedactingAKeyTokenDoesNotBlindTheShapeRulesToItsValueOnTheStream(t *testing.T) {
	const workload = "correcthorsebatterystaple"

	for _, carrier := range invisibleCarriers {
		t.Run(carrier.name, func(t *testing.T) {
			key := splitBy(carrier.carrier, "pass", "word")
			emitter, path := openWith(t, diagnostics.NewRedactor([]string{"password"}))

			emitter.Emit(events.Event{Kind: events.Reverted, Reason: key + ": |\n  " + workload})

			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if strings.Contains(string(raw), workload) {
				t.Fatalf("the workload credential reached the stream verbatim: %s", raw)
			}
		})
	}
}

// The shape rules match a span rather than a whole configured value, so a secret
// whose head is credential shaped leaves a scrub-first sink as "[REDACTED]"
// followed by its own tail.
func TestAConfiguredSecretWhoseHeadIsCredentialShapedIsRedactedWholeFromTheStream(t *testing.T) {
	const (
		password = "AKIAIOSFODNN7EXAMPLEtailofthesecret"
		tail     = "tailofthesecret"
	)
	emitter, path := openWith(t, diagnostics.NewRedactor([]string{password}))

	emitter.Emit(events.Event{Kind: events.Failed, Reason: "the pull used " + password})

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(raw), tail) {
		t.Fatalf("a shape rule ate the head and left the tail: %s", raw)
	}
}

// C-L130's closures, pinned on the stream: a shape credential ops-pilot never
// held is still scrubbed when a rune splits it, and a configured value that
// carries such a rune is still matched in both of the forms it is stored under.
func TestTheInvisibleRuneClosuresHoldAtTheStreamSink(t *testing.T) {
	carrying := splitBy(0x200B, "correcthorse", "batterystaple")
	shapeSplit := splitBy(0x200B, "AKIA", "IOSFODNN7EXAMPLE")

	tests := []struct {
		name       string
		configured []string
		reason     string
		forbidden  string
	}{
		{
			name:       "a shape credential split by an invisible rune",
			configured: []string{"a-configured-value"},
			reason:     "the pod printed " + shapeSplit,
			forbidden:  "AKIAIOSFODNN7EXAMPLE",
		},
		{
			name:       "a configured secret carrying a rune, quoted verbatim",
			configured: []string{carrying},
			reason:     "the pod printed " + carrying,
			forbidden:  "correcthorsebatterystaple",
		},
		{
			name:       "a configured secret carrying a rune, quoted rendered",
			configured: []string{carrying},
			reason:     "the pod printed correcthorsebatterystaple",
			forbidden:  "correcthorsebatterystaple",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			emitter, path := openWith(t, diagnostics.NewRedactor(test.configured))

			emitter.Emit(events.Event{Kind: events.Diagnosed, Reason: test.reason})

			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if strings.Contains(string(raw), test.forbidden) {
				t.Fatalf("the stream kept %q: %s", test.forbidden, raw)
			}
		})
	}
}

// Redacting an object reference or a SHA is silent and permanent for that
// object, and the fields a consumer matches on are exactly those.
func TestScrubbingLeavesTheFieldsAConsumerParsesOnStructure(t *testing.T) {
	const (
		sha = "9f8d7c6b5a4938271605f4e3d2c1b0a998877665"
		ref = "production-workloads/HelmRelease/sonarrx"
	)
	emitter, path := open(t)
	emitter.Emit(events.Event{
		Kind:        events.Reverted,
		PullRequest: 714,
		Dependency:  "ghcr.io/home-operations/secret-key:v4.5.6",
		From:        "1.2.3",
		To:          "4.5.6",
		Decision:    "merge",
		Watch:       "fail",
		Action:      "revert",
		Label:       "ops-pilot/reverted",
		SHA:         sha,
		Reason:      "media/HelmRelease/sonarr did not become ready",
		Objects: events.Objects([]domain.ObjectHealth{{
			Ref:    domain.ObjectRef{Kind: "HelmRelease", Namespace: "production-workloads", Name: "sonarrx"},
			Reason: "install retries exhausted",
		}}),
	})

	event := lines(t, path)[0]
	for field, want := range map[string]string{
		"dependency": "ghcr.io/home-operations/secret-key:v4.5.6",
		"from":       "1.2.3",
		"to":         "4.5.6",
		"decision":   "merge",
		"watch":      "fail",
		"action":     "revert",
		"label":      "ops-pilot/reverted",
		"sha":        sha,
		"repository": "a/b",
		"runId":      "run-1",
		"reason":     "media/HelmRelease/sonarr did not become ready",
	} {
		if event[field] != want {
			t.Errorf("%s: want %q, got %v", field, want, event[field])
		}
	}
	object := event["objects"].([]any)[0].(map[string]any)
	if object["ref"] != ref {
		t.Errorf("ref: want %q, got %v", ref, object["ref"])
	}
	if object["reason"] != "install retries exhausted" {
		t.Errorf("reason: got %v", object["reason"])
	}
}

// scrubbed and structural are the hand-made decisions the guards below check.
// Declaring a field here is a claim about the emitted stream, not a note, so the
// two guards read the same maps: one insists a decision exists, the other
// insists the scrubbed decision is carried out.
var (
	scrubbed = map[string]bool{
		"Event.Reason": true, "Event.Error": true, "Object.Reason": true,
	}
	structural = map[string]bool{
		"Event.Kind": true, "Event.RunID": true, "Event.Repository": true,
		"Event.Dependency": true, "Event.From": true, "Event.To": true,
		"Event.Decision": true, "Event.Watch": true,
		"Event.Action": true, "Event.Label": true, "Event.SHA": true,
		"Object.Ref": true,
	}
)

// A field added later is scrubbed or exempt by a decision someone made, not by
// forgetting: the scrubbed set is enumerated by hand and cannot follow the type.
func TestEveryStringFieldIsEitherScrubbedOrDeclaredStructural(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var walk func(shape reflect.Type, name string)
	walk = func(shape reflect.Type, name string) {
		switch shape.Kind() {
		case reflect.String:
			if !scrubbed[name] && !structural[name] {
				t.Errorf("%s is neither scrubbed nor declared structural", name)
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
	walk(reflect.TypeOf(events.Event{}), "Event")
	for name := range scrubbed {
		if structural[name] {
			t.Errorf("%s is declared both scrubbed and structural", name)
		}
	}
}

// Declaring a field scrubbed was a green no-op: scrub() names the fields it
// touches by hand, and nothing connected the map to what it actually does, so a
// new field could be declared scrubbed and emitted raw with the suite passing.
// The assertion is driven off the map instead - every name in it is planted with
// a secret and the stream is read back.
func TestEveryFieldDeclaredScrubbedIsScrubbedInTheEmittedLine(t *testing.T) {
	const secret = "Ai8fkq2LmZx0Rt7Yb3Nc"

	for name := range scrubbed {
		t.Run(name, func(t *testing.T) {
			event := events.Event{Kind: events.Diagnosed}
			if !plant(reflect.ValueOf(&event).Elem(), "Event", name, "quoted api_key="+secret) {
				t.Fatalf("%s is declared scrubbed but no such field exists on the type", name)
			}
			emitter, path := open(t)
			emitter.Emit(event)

			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if strings.Contains(string(raw), secret) {
				t.Fatalf("%s is declared scrubbed and reached the stream raw: %s", name, raw)
			}
			if !strings.Contains(string(raw), "[REDACTED]") {
				t.Fatalf("%s vanished instead of being redacted: %s", name, raw)
			}
		})
	}
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
		return plant(value.Index(0), name, target, text)
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

// Emit takes the event by value, but Objects shares its backing array with the
// caller, who still renders the same failures to stdout and the history DB.
func TestEmitDoesNotRewriteTheCallersObjects(t *testing.T) {
	emitter, _ := open(t)
	objects := events.Objects([]domain.ObjectHealth{{
		Ref:    domain.ObjectRef{Kind: "HelmRelease", Namespace: "media", Name: "sonarr"},
		Reason: "failed: api_key=Ai8fkq2LmZx0Rt7Yb3Nc",
	}})
	emitter.Emit(events.Event{Kind: events.Reverted, Objects: objects})

	if objects[0].Reason != "failed: api_key=Ai8fkq2LmZx0Rt7Yb3Nc" {
		t.Fatalf("Emit rewrote the caller's slice: %q", objects[0].Reason)
	}
}

var rightToLeftOverride = string(rune(0x202e))

var strippingCorpus = []string{
	"", "plain text", "fix: bump nginx 1.2.3 -> 1.2.4",
	"--- a/values.yaml\n+++ b/values.yaml\n@@ -1 +1 @@\n-\ttag: 1.2.3\n+\ttag: 1.2.4\n",
	"safe\x1b[2K\rIMPOSTOR", "wake\aup", "first\rsecond", "one\ntwo",
	"report" + rightToLeftOverride + "txt.exe",
	"zero" + string(rune(0x200b)) + "width",
	"join" + string(rune(0x200d)) + "er",
	"line" + string(rune(0x2028)) + "sep",
	"para" + string(rune(0x2029)) + "sep",
	"bad" + string(utf8.RuneError) + "rune",
	"priv" + string(rune(0xe000)) + "ate",
	"media/HelmRelease/sonarr は準備できませんでした",
	"rollout stalled 🚀 after 3 attempts — see #1204 (café, naïve)",
	"\x00\x01\x1f\x7f", "tab\tsep",
}

// JSON escaping makes an escape sequence inert in the file, but the stream is
// read with `jq -r`, which hands the decoded bytes to the terminal.
func TestTerminalControlNeverSurvivesDecodingTheStream(t *testing.T) {
	tests := []struct {
		name  string
		event events.Event
		field func(map[string]any) any
	}{
		{
			name:  "an erase-and-rewrite in a halt reason",
			event: events.Event{Kind: events.Halted, Reason: "safe\x1b[2K\rIMPOSTOR"},
			field: func(event map[string]any) any { return event["reason"] },
		},
		{
			name:  "a bell and a bidi override in an error",
			event: events.Event{Kind: events.Failed, Error: "wake\aup report" + rightToLeftOverride + "txt.exe"},
			field: func(event map[string]any) any { return event["error"] },
		},
		{
			name: "a controller message that repaints the line",
			event: events.Event{Kind: events.Reverted, Objects: events.Objects([]domain.ObjectHealth{{
				Ref:    domain.ObjectRef{Kind: "HelmRelease", Namespace: "media", Name: "sonarr"},
				Reason: "install retries exhausted\x1b[1;1HREADY",
			}})},
			field: func(event map[string]any) any {
				return event["objects"].([]any)[0].(map[string]any)["reason"]
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			emitter, path := open(t)
			emitter.Emit(test.event)

			value, _ := test.field(lines(t, path)[0]).(string)
			for _, r := range value {
				if r == '\n' || r == '\t' {
					continue
				}
				if r == 0x1b || unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Co, unicode.Cs) {
					t.Fatalf("%q survived into %q", r, value)
				}
			}
		})
	}
}

// The stream once carried its own copy of the strip rule. A copy that drifts
// from the one the history database uses means the same reason is stored two
// ways and only one of them was reviewed.
func TestTheStreamStripsProseWithTheSharedRuleAndNoOther(t *testing.T) {
	for _, text := range strippingCorpus {
		emitter, path := open(t)
		emitter.Emit(events.Event{Kind: events.Halted, Reason: text})

		want := diagnostics.Storable(text)
		if got, _ := lines(t, path)[0]["reason"].(string); got != want {
			t.Errorf("the stream diverged from diagnostics.Storable for %q:\n got %q\nwant %q",
				text, got, want)
		}
	}
}

// A stored reason is often the diff an agent wrote, where the line and column
// structure is the content: stripping it would leave an unreadable record.
func TestAMultiLineDiffReachesTheStreamByteExact(t *testing.T) {
	const diff = "--- a/values.yaml\n" +
		"+++ b/values.yaml\n" +
		"@@ -12,3 +12,3 @@\n" +
		" image:\n" +
		"-\ttag: 1.2.3\n" +
		"+\ttag: 1.2.4\n"

	emitter, path := open(t)
	emitter.Emit(events.Event{Kind: events.FixApplied, Reason: diff})

	if got, _ := lines(t, path)[0]["reason"].(string); got != diff {
		t.Fatalf("the diff was mangled:\n got %q\nwant %q", got, diff)
	}
}

// The strip is a rune-category rule, so prose in any script has to come back
// unchanged; only the invisible formatting runes are house-forbidden.
func TestNonLatinProseReachesTheStreamUnchanged(t *testing.T) {
	for _, text := range []string{
		"media/HelmRelease/sonarr は準備できませんでした",
		"فشل تحديث الحزمة بعد ثلاث محاولات",
		"rollout stalled 🚀 after 3 attempts — see #1204 (café, naïve)",
		"tag: v1.2.3-rc.1+build.7",
	} {
		emitter, path := open(t)
		emitter.Emit(events.Event{Kind: events.Halted, Reason: text})
		if got, _ := lines(t, path)[0]["reason"].(string); got != text {
			t.Errorf("legitimate prose was altered:\n got %q\nwant %q", got, text)
		}
	}
}

// No --events flag is the normal case.
func TestNoPathYieldsAUsableNilEmitter(t *testing.T) {
	emitter, err := events.Open("", nil, nil)
	if err != nil || emitter != nil {
		t.Fatalf("want a nil emitter and no error, got %v %v", emitter, err)
	}
	emitter.Bind(domain.Run{})
	emitter.Emit(events.Event{Kind: events.RunStarted})
	if emitter.Err() != nil || emitter.Close() != nil {
		t.Fatal("a nil emitter must be safe to use")
	}
}
