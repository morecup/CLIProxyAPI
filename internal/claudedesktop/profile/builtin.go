package profile

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed v140609.bundle.json
var builtinV140609JSON []byte

func BuiltinV140609() (*Bundle, error) {
	var bundle Bundle
	if errUnmarshal := json.Unmarshal(builtinV140609JSON, &bundle); errUnmarshal != nil {
		return nil, fmt.Errorf("decode built-in Claude Desktop profile: %w", errUnmarshal)
	}
	installV140609ObservedExecutableMappings(&bundle)
	if errValidate := bundle.Validate(); errValidate != nil {
		return nil, errValidate
	}
	return &bundle, nil
}
