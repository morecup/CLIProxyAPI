package signature

import "strings"

type SignatureProvider string

const (
	SignatureProviderUnknown SignatureProvider = "unknown"
	SignatureProviderClaude  SignatureProvider = "claude"
)

type SignatureBlockKind string

const (
	SignatureBlockKindUnknown        SignatureBlockKind = "unknown"
	SignatureBlockKindClaudeThinking SignatureBlockKind = "claude_thinking"
)

type SignatureCompatibilityAction string

const (
	SignatureActionPreserve                SignatureCompatibilityAction = "preserve"
	SignatureActionDropBlock               SignatureCompatibilityAction = "drop_block"
	SignatureActionDropSignature           SignatureCompatibilityAction = "drop_signature"
	SignatureActionNoCompatibleReplacement SignatureCompatibilityAction = "no_compatible_replacement"
)

type SignatureCompatibilityDecision struct {
	TargetProvider       SignatureProvider
	DetectedProvider     SignatureProvider
	BlockKind            SignatureBlockKind
	Compatible           bool
	Action               SignatureCompatibilityAction
	ReplacementSignature string
	NormalizedSignature  string
	Reason               string
}

// SignatureProviderFromModelName maps common model names to the provider family
// whose signed history can be safely replayed for that model.
func SignatureProviderFromModelName(modelName string) SignatureProvider {
	if strings.Contains(strings.ToLower(strings.TrimSpace(modelName)), "claude") {
		return SignatureProviderClaude
	}
	return SignatureProviderUnknown
}

// DetectSignatureProvider classifies the provider family that can replay
// rawSignature.
func DetectSignatureProvider(rawSignature string) SignatureProvider {
	return DetectSignatureProviderForBlock(rawSignature, SignatureBlockKindUnknown)
}

// DetectSignatureProviderForBlock classifies rawSignature with block-kind
// context.
func DetectSignatureProviderForBlock(rawSignature string, _ SignatureBlockKind) SignatureProvider {
	sig := strings.TrimSpace(rawSignature)
	if sig == "" {
		return SignatureProviderUnknown
	}

	if prefixedProvider, unprefixed, ok := SplitSignatureProviderPrefix(sig); ok {
		if prefixedProvider == SignatureProviderClaude &&
			(IsValidClaudeThinkingSignature(unprefixed, ClaudeSignatureValidationOptions{Strict: true}) || IsValidClaudeCAISSignature(unprefixed)) {
			return SignatureProviderClaude
		}
		return SignatureProviderUnknown
	}
	if strings.Contains(sig, "#") {
		return SignatureProviderUnknown
	}

	if IsValidClaudeCAISSignature(sig) {
		return SignatureProviderClaude
	}
	if IsValidClaudeThinkingSignature(sig, ClaudeSignatureValidationOptions{Strict: true}) {
		return SignatureProviderClaude
	}
	return SignatureProviderUnknown
}

func IsSignatureCompatibleWithProvider(targetProvider SignatureProvider, rawSignature string) bool {
	decision := DecideSignatureCompatibility(targetProvider, rawSignature, SignatureBlockKindUnknown)
	return decision.Compatible
}

// DecideSignatureCompatibility returns the safe handling policy for replaying a
// signed block into targetProvider.
func DecideSignatureCompatibility(targetProvider SignatureProvider, rawSignature string, blockKind SignatureBlockKind) SignatureCompatibilityDecision {
	return DecideSignatureCompatibilityForModel(targetProvider, "", rawSignature, blockKind)
}

// DecideSignatureCompatibilityForModel returns the safe handling policy for replaying a
// signed block into targetProvider for targetModel.
func DecideSignatureCompatibilityForModel(targetProvider SignatureProvider, targetModel string, rawSignature string, blockKind SignatureBlockKind) SignatureCompatibilityDecision {
	if blockKind == "" {
		blockKind = SignatureBlockKindUnknown
	}

	detected := DetectSignatureProviderForBlock(rawSignature, blockKind)
	decision := SignatureCompatibilityDecision{
		TargetProvider:   targetProvider,
		DetectedProvider: detected,
		BlockKind:        blockKind,
	}

	if detected == SignatureProviderClaude {
		compatible, constrained, reason := claudeModelSignatureCompatibility(rawSignature, targetModel)
		if constrained && !compatible {
			decision.Compatible = false
			decision.Action = SignatureActionDropBlock
			decision.Reason = reason
			return decision
		}
		if constrained {
			decision.Reason = reason
		}
		decision.Compatible = true
		decision.Action = SignatureActionPreserve
		decision.NormalizedSignature = normalizeCompatibleSignatureForProvider(targetProvider, rawSignature)
		if decision.Reason == "" {
			decision.Reason = claudeCompatibleSignatureReason(targetProvider, rawSignature, targetModel)
		}
		return decision
	}

	decision.Compatible = false
	if targetProvider == SignatureProviderClaude {
		decision.Action = SignatureActionDropBlock
		decision.Reason = "Claude has no cross-provider bypass sentinel for thinking blocks"
	} else {
		decision.Action = SignatureActionNoCompatibleReplacement
		decision.Reason = "unknown target provider"
	}
	return decision
}

func SplitSignatureProviderPrefix(rawSignature string) (SignatureProvider, string, bool) {
	prefix, rest, ok := strings.Cut(strings.TrimSpace(rawSignature), "#")
	if !ok {
		return SignatureProviderUnknown, rawSignature, false
	}
	provider := SignatureProviderFromCachePrefix(prefix)
	if provider == SignatureProviderUnknown {
		return SignatureProviderUnknown, rawSignature, false
	}
	return provider, strings.TrimSpace(rest), true
}

// SignatureProviderFromCachePrefix maps this repo's explicit provider-prefix
// envelope to a provider family. This is intentionally stricter than
// SignatureProviderFromModelName so arbitrary model names such as
// "claude-cache#..." cannot be mistaken for trusted provider provenance.
func SignatureProviderFromCachePrefix(prefix string) SignatureProvider {
	switch strings.ToLower(strings.TrimSpace(prefix)) {
	case "claude", "anthropic", "cais", "claude-cais", "claude_cais", "ccmax", "claude-code-max", "claude_code_max":
		return SignatureProviderClaude
	default:
		return SignatureProviderUnknown
	}
}

// SignaturePayloadWithoutProviderPrefix strips this repo's provider cache prefix
// when present. The returned string is the value that should be replayed to an
// upstream provider.
func SignaturePayloadWithoutProviderPrefix(rawSignature string) string {
	if _, unprefixed, ok := SplitSignatureProviderPrefix(rawSignature); ok {
		return unprefixed
	}
	return strings.TrimSpace(rawSignature)
}

// CompatibleSignatureForProvider returns a replayable provider-native signature
// for targetProvider. It strips this repo's provider prefix and normalizes
// Claude signatures to the format expected by the target when possible.
func CompatibleSignatureForProvider(targetProvider SignatureProvider, rawSignature string) (string, bool) {
	return CompatibleSignatureForProviderBlock(targetProvider, rawSignature, SignatureBlockKindUnknown)
}

// CompatibleSignatureForProviderBlock returns a replayable provider-native
// signature for targetProvider when the source block kind is known.
func CompatibleSignatureForProviderBlock(targetProvider SignatureProvider, rawSignature string, blockKind SignatureBlockKind) (string, bool) {
	decision := DecideSignatureCompatibility(targetProvider, rawSignature, blockKind)
	if !decision.Compatible || decision.NormalizedSignature == "" {
		return "", false
	}
	return decision.NormalizedSignature, true
}

// claudeModelSignatureCompatibility applies the model-level replay matrix carried by
// model-tagged Claude signatures. Both CAIS and newer classic E/R envelopes can
// carry model_text. Signatures without a model id deliberately remain
// provider-compatible rather than being dropped on an inference the payload
// cannot support.
func claudeModelSignatureCompatibility(rawSignature, targetModel string) (compatible, constrained bool, reason string) {
	source, modelTagged := claudeSignatureModelID(rawSignature)
	if !modelTagged {
		return true, false, ""
	}

	target := normalizeClaudeSignatureModelID(targetModel)
	if target == "" {
		return true, false, ""
	}

	if target == "claude-opus-5-5" && !claudeOpus55ReadableSourceModel(source) {
		return false, true, "Claude Opus 5.5 can replay thinking blocks from Claude Opus 5.5, Opus 5, and earlier Opus, Sonnet, or Haiku models; source model is " + source
	}

	if source == "claude-opus-5-5" {
		switch target {
		case "claude-opus-5-5", "claude-fable-5-1", "claude-mythos-5-1":
			return true, true, "Claude Opus 5.5 CAIS signature is compatible with target model " + target
		default:
			return false, true, "Claude Opus 5.5 CAIS signatures can only be replayed by Claude Opus 5.5, Fable 5.1, or Mythos 5.1; target model is " + target
		}
	}

	return true, false, ""
}

func claudeOpus55ReadableSourceModel(source string) bool {
	lower := strings.ToLower(strings.TrimSpace(source))
	for _, marker := range []string{
		"claude-opus-5-5",
		"claude-opus-5",
		"claude-opus-4",
		"claude-3-opus",
		"claude-sonnet-5",
		"claude-sonnet-4",
		"claude-3-7-sonnet",
		"claude-3-5-sonnet",
		"claude-3-sonnet",
		"claude-haiku-4",
		"claude-3-5-haiku",
		"claude-3-haiku",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func claudeSignatureModelID(rawSignature string) (string, bool) {
	payload := stripClaudeSignaturePrefix(SignaturePayloadWithoutProviderPrefix(rawSignature))
	if payload == "" {
		return "", false
	}
	if info, err := InspectClaudeCAISSignature(payload); err == nil {
		return normalizeClaudeSignatureModelID(info.ModelText), true
	}

	var (
		tree *ClaudeSignatureTree
		err  error
	)
	switch payload[0] {
	case 'E':
		tree, err = InspectClaudeSingleLayerSignature(payload)
	case 'R':
		tree, err = InspectClaudeDoubleLayerSignature(payload)
	default:
		return "", false
	}
	if err != nil || tree == nil || strings.TrimSpace(tree.ModelText) == "" {
		return "", false
	}
	return normalizeClaudeSignatureModelID(tree.ModelText), true
}

func normalizeClaudeSignatureModelID(model string) string {
	lower := strings.ToLower(strings.TrimSpace(model))
	switch {
	case lower == "opus", strings.Contains(lower, "claude-opus-5-5"):
		return "claude-opus-5-5"
	case strings.Contains(lower, "claude-fable-5-1"):
		return "claude-fable-5-1"
	case strings.Contains(lower, "claude-mythos-5-1"):
		return "claude-mythos-5-1"
	default:
		return lower
	}
}

// claudeCompatibleSignatureReason explains why a matching signature is
// replayable. Claude CAIS signatures carry the issuing model inside the payload,
// so the embedded model and the target model are both reported to make signature
// decisions traceable in debug logs.
func claudeCompatibleSignatureReason(targetProvider SignatureProvider, rawSignature, targetModel string) string {
	const genericReason = "signature provider matches target provider"
	if targetProvider != SignatureProviderClaude {
		return genericReason
	}
	info, err := InspectClaudeCAISSignature(SignaturePayloadWithoutProviderPrefix(rawSignature))
	if err != nil {
		return genericReason
	}
	reason := "valid Claude CAIS signature with embedded model " + info.ModelText + " matches the Claude provider"
	if trimmedModel := strings.TrimSpace(targetModel); trimmedModel != "" {
		reason += " and is compatible with target model " + trimmedModel
	}
	return reason
}

func normalizeCompatibleSignatureForProvider(targetProvider SignatureProvider, rawSignature string) string {
	payload := SignaturePayloadWithoutProviderPrefix(rawSignature)
	if targetProvider != SignatureProviderClaude {
		return ""
	}
	if IsValidClaudeCAISSignature(payload) {
		return payload
	}
	normalized, err := NormalizeClaudeProviderNativeThinkingSignature(payload)
	if err != nil {
		return ""
	}
	return normalized
}
