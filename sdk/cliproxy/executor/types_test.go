package executor

import (
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestResponseFormatOrSourceUsesExplicitResponseFormat(t *testing.T) {
	opts := Options{
		SourceFormat:   sdktranslator.FormatOpenAI,
		ResponseFormat: sdktranslator.FormatClaude,
	}

	if got := ResponseFormatOrSource(opts); got != sdktranslator.FormatClaude {
		t.Fatalf("ResponseFormatOrSource() = %q, want %q", got, sdktranslator.FormatClaude)
	}
}

func TestResponseFormatOrSourceFallsBackToSourceFormat(t *testing.T) {
	opts := Options{SourceFormat: sdktranslator.FormatOpenAIResponse}

	if got := ResponseFormatOrSource(opts); got != sdktranslator.FormatOpenAIResponse {
		t.Fatalf("ResponseFormatOrSource() = %q, want %q", got, sdktranslator.FormatOpenAIResponse)
	}
}
