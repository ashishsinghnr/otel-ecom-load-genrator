package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The shipped topologies must load and validate. This is the test that fails
// if a schema change breaks the files users actually run.
func TestShippedTopologies(t *testing.T) {
	files, err := filepath.Glob("../../topologies/*.json")
	if err != nil {
		t.Fatalf("globbing topologies: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no topology files found; expected at least shop-full.json and shop-smoke.json")
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			f, warnings, err := Load(path)
			if err != nil {
				t.Fatalf("failed to load: %v", err)
			}
			// A shipped topology should be warning-free; warnings usually mean
			// an unreachable service or weights that do not sum to 100.
			for _, w := range warnings {
				t.Errorf("unexpected warning: %s", w)
			}
			if len(f.Topology.Services) == 0 {
				t.Error("no services")
			}
			if len(f.RootRoutes) == 0 {
				t.Error("no root routes")
			}
		})
	}
}

// Unknown fields must be rejected, so a typo in a key is caught at load time
// rather than being silently ignored.
func TestLoad_RejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	writeFile(t, path, `{
      "topology": {"services": [{"serviceName": "a", "instnaces": ["a-1"]}]},
      "rootRoutes": []
    }`)

	if _, _, err := Load(path); err == nil {
		t.Fatal("expected a misspelled key to be rejected")
	}
}

func TestLoad_MissingFile(t *testing.T) {
	if _, _, err := Load(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("expected a missing file to error")
	}
}

func TestLoad_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	writeFile(t, path, `{"topology": `)

	if _, _, err := Load(path); err == nil {
		t.Fatal("expected malformed JSON to error")
	}
}

// Load must run validation, not just parse.
func TestLoad_RunsValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dangling.json")
	writeFile(t, path, `{
      "topology": {"services": [{
        "serviceName": "a",
        "instances": ["a-1"],
        "routes": [{
          "route": "r",
          "downstreamCalls": {"ghost": "x"},
          "latency": {"p50": 1, "p99": 2, "outlierMultiplier": 1}
        }]
      }]},
      "rootRoutes": [{"service": "a", "route": "r", "tracesPerHour": 1}]
    }`)

	_, _, err := Load(path)
	if err == nil {
		t.Fatal("expected a dangling reference to be rejected by Load")
	}
}

// writeFile writes test fixture content, failing the test on error.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
}
