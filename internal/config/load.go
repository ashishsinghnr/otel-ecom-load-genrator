package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// Load reads and validates a topology file. Warnings are returned for the
// caller to log; a non-nil error means the topology must not be used.
func Load(path string) (*File, []string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("reading topology %s: %w", path, err)
	}

	var f File
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, nil, fmt.Errorf("parsing topology %s: %w", path, err)
	}

	warnings, err := ValidateWithWarnings(&f)
	if err != nil {
		return nil, warnings, fmt.Errorf("invalid topology %s: %w", path, err)
	}
	return &f, warnings, nil
}
