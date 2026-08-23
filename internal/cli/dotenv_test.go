package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseDotenv(t *testing.T) {
	got, err := parseDotenv(strings.NewReader("\n# comment\nA=one\nexport B = ' two '\nC=\"three#four\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "one", "B": " two ", "C": "three#four"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dotenv = %#v, want %#v", got, want)
	}
}

func TestParseDotenvRejectsMalformedSyntaxWithoutDisclosingValues(t *testing.T) {
	for _, test := range []struct {
		name, input, category string
	}{
		{"invalid name", "A-B=secret-fixture\n", "name"},
		{"missing equals", "TOKEN secret-fixture\n", "="},
		{"unmatched quote", "TOKEN='secret-fixture\n", "quote"},
		{"text after quote", "TOKEN='value' secret-fixture\n", "quote"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseDotenv(strings.NewReader(test.input))
			if err == nil || !strings.Contains(err.Error(), "line 1") || !strings.Contains(err.Error(), test.category) {
				t.Fatalf("error = %v", err)
			}
			if strings.Contains(err.Error(), "secret-fixture") {
				t.Fatalf("error disclosed dotenv value: %v", err)
			}
		})
	}
}

func TestLoadDotenvAcceptsMissingFile(t *testing.T) {
	values, err := loadDotenv(filepath.Join(t.TempDir(), ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if values != nil {
		t.Fatalf("values = %#v, want nil", values)
	}
}

func TestLoadDotenvReturnsOpenError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loop")
	if err := os.Symlink(path, path); err != nil {
		t.Skipf("create self-referential symlink: %v", err)
	}

	_, err := loadDotenv(path)
	if err == nil || !strings.Contains(err.Error(), "open dotenv") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadDotenvReturnsReadError(t *testing.T) {
	_, err := loadDotenv(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "read dotenv") {
		t.Fatalf("error = %v", err)
	}
}

func TestEnvironmentLookupPrefersPrimary(t *testing.T) {
	lookup := environmentLookup(func(name string) (string, bool) {
		if name == "VALUE" {
			return "", true
		}
		return "", false
	}, map[string]string{"VALUE": "dotenv", "FALLBACK": "dotenv"})
	if value, present := lookup("VALUE"); !present || value != "" {
		t.Fatalf("primary empty value = %q, %v", value, present)
	}
	if value, present := lookup("FALLBACK"); !present || value != "dotenv" {
		t.Fatalf("fallback = %q, %v", value, present)
	}
}
