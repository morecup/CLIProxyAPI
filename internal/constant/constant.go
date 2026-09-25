// Package constant defines provider name constants used throughout the CLI Proxy API.
// These constants identify different AI service providers and their variants,
// ensuring consistent naming across the application.
package constant

const (
	// Claude represents the Anthropic Claude provider identifier.
	Claude = "claude"

	// AnthropicCompatible represents generic Anthropic Messages API credentials.
	// It is deliberately separate from the Desktop-only Claude provider.
	AnthropicCompatible = "anthropic-compatible"

	// OpenAI represents the OpenAI provider identifier.
	OpenAI = "openai"

	// OpenaiResponse represents the OpenAI response format identifier.
	OpenaiResponse = "openai-response"
)
