package profile

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed v140609.bundle.json
var builtinV140609JSON []byte

//go:embed v2255313.request-profile.json
var builtinV2255313RequestProfileJSON []byte

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

func BuiltinCurrent() (*Bundle, error) {
	bundle, errBundle := BuiltinV140609()
	if errBundle != nil {
		return nil, errBundle
	}
	var requestProfile RequestProfile
	if errUnmarshal := json.Unmarshal(builtinV2255313RequestProfileJSON, &requestProfile); errUnmarshal != nil {
		return nil, fmt.Errorf("decode built-in current Claude Desktop request profile: %w", errUnmarshal)
	}
	bundle.RequestProfiles = append(bundle.RequestProfiles, requestProfile)
	if errValidate := bundle.Validate(); errValidate != nil {
		return nil, errValidate
	}
	return bundle, nil
}
