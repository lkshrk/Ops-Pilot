package flux

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"testing"
)

// An adapter that reads the operator's YAML shape can only be reused or
// retargeted by widening that shape, so the rule is that adapters take their own
// inputs and the composition root maps.
func TestFluxDoesNotImportTheOperatorConfigPackage(t *testing.T) {
	const forbidden = "github.com/lkshrk/ops-pilot/internal/config"

	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list the package sources: %v", err)
	}
	if len(sources) == 0 {
		t.Fatal("want the package sources, found none")
	}

	fset := token.NewFileSet()
	for _, source := range sources {
		file, err := parser.ParseFile(fset, source, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", source, err)
		}
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("parse the import path %s in %s: %v", spec.Path.Value, source, err)
			}
			if path == forbidden {
				t.Errorf("%s imports %s", source, forbidden)
			}
		}
	}
}
