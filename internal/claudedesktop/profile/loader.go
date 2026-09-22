package profile

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Load reads a sanitized immutable bundle. Raw recorder directories are not a
// supported input; compilation and secret scanning stay in the knowledge kit.
func Load(path string) (*Bundle, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return BuiltinV140609()
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		return nil, fmt.Errorf("read Claude Desktop profile %q: %w", path, errRead)
	}
	var bundle Bundle
	if errUnmarshal := json.Unmarshal(data, &bundle); errUnmarshal != nil {
		return nil, fmt.Errorf("decode Claude Desktop profile %q: %w", path, errUnmarshal)
	}
	installV140609ObservedExecutableMappings(&bundle)
	if errValidate := bundle.Validate(); errValidate != nil {
		return nil, errValidate
	}
	return &bundle, nil
}
