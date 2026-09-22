package prompt

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"
)

// Submission is a scalar-only observation of the proxy's input boundary. It
// does not claim to reconstruct the caller's hidden SDK objects or hooks.
type Submission struct {
	ObservedAt                        time.Time
	Known, FreshConversation          bool
	IsNegative, IsKeepGoing, IsWakeup bool
	// IsMeta marks a locally owned SendMessage delivery to the main
	// conversation; its input event carries no prompt index.
	IsMeta bool
	Length int
}

var inputNegative = regexp.MustCompile(`\b(wtf|wth|ffs|omfg|shit(ty|tiest)?|dumbass|horrible|awful|piss(ed|ing)? off|piece of (shit|crap|junk)|what the (fuck|hell)|fucking? (broken|useless|terrible|awful|horrible)|fuck you|screw (this|you)|so frustrating|this sucks|damn it)\b`)
var inputContinue = regexp.MustCompile(`\b(keep going|go on)\b`)
var inputWakePunctuation = regexp.MustCompile(`^(\?+|\.{2,}|…+)$`)
var inputWakeGreeting = regexp.MustCompile(`^(hi+|hello+|he+y+|yo+)$`)
var inputWakeWords = map[string]bool{
	"ping": true, "u there": true, "you there": true, "are you there": true, "r u there": true,
	"u here": true, "you here": true, "are you here": true, "you back": true, "are you back": true,
	"u back": true, "anyone there": true, "anybody there": true, "still there": true,
	"you still there": true, "are you still there": true, "still working": true,
	"you still working": true, "are you still working": true, "you stuck": true,
	"are you stuck": true, "u stuck": true, "喂": true, "在吗": true, "在嗎": true,
	"还在吗": true, "還在嗎": true, "在不在": true, "もしもし": true, "おい": true,
	"おーい": true, "いますか": true, "여보세요": true, "야": true, "있어요": true,
	"hola": true, "oye": true, "estás ahí": true, "estas ahi": true, "sigues ahí": true,
	"sigues ahi": true, "oi": true, "olá": true, "ola": true, "alô": true, "alo": true,
	"tá aí": true, "ta ai": true, "está aí": true, "esta ai": true, "allo": true,
	"allô": true, "salut": true, "coucou": true, "t'es là": true, "t'es la": true,
	"tu es là": true, "tu es la": true, "hallo": true, "bist du da": true,
	"noch da": true, "привет": true, "эй": true, "алло": true, "ты тут": true,
	"ciao": true, "ehi": true, "ci sei": true,
}

// ObserveSubmission must run on translated input before thinking, payload,
// system and cache transformations. It never retains text, images or tool data.
// Ambiguous merged rows, tool submissions and slash dispatch remain unknown.
func ObserveSubmission(body []byte, at time.Time) Submission {
	result := Submission{ObservedAt: at}
	var root struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &root) != nil || len(root.Messages) == 0 {
		return result
	}
	last := root.Messages[len(root.Messages)-1]
	if last.Role != "user" || (len(root.Messages) > 1 && root.Messages[len(root.Messages)-2].Role == "user") {
		return result
	}
	var firstText, lastText string
	if json.Unmarshal(last.Content, &firstText) == nil {
		lastText = firstText
		if trimInputSpace(firstText) == "" {
			return result
		}
	} else {
		var blocks []struct {
			Type string  `json:"type"`
			Text *string `json:"text"`
		}
		if json.Unmarshal(last.Content, &blocks) != nil || blocks == nil {
			return result
		}
		seen := false
		for _, block := range blocks {
			switch block.Type {
			case "text":
				if block.Text == nil {
					return result
				}
				if !seen {
					firstText, seen = *block.Text, true
				}
				lastText = *block.Text
			case "image", "document":
			default:
				return result
			}
		}
	}
	if strings.HasPrefix(trimInputSpace(firstText), "/") {
		return result
	}
	result.Known, result.FreshConversation = true, len(root.Messages) == 1
	result.Length = len(utf16.Encode([]rune(lastText)))
	lower := lowerInputText(firstText)
	result.IsNegative = inputNegative.MatchString(lower)
	trimmed := trimInputSpace(lower)
	result.IsKeepGoing = trimmed == "continue" || inputContinue.MatchString(trimmed)
	wake := trimInputSpace(firstText)
	if len(utf16.Encode([]rune(wake))) <= 40 {
		word := lowerInputText(wake)
		word = strings.TrimLeft(word, "¿¡")
		word = trimInputSpace(strings.TrimRight(word, "?!.¿¡？！。…"))
		result.IsWakeup = inputWakePunctuation.MatchString(wake) || inputWakeGreeting.MatchString(word) || inputWakeWords[word]
	}
	return result
}

// ECMAScript trim includes FEFF but excludes NEL (0085).
func trimInputSpace(value string) string {
	return strings.TrimFunc(value, func(r rune) bool {
		return r == '\t' || r == '\n' || r == '\v' || r == '\f' || r == '\r' || r == ' ' || r == 0xA0 ||
			r == 0x1680 || (r >= 0x2000 && r <= 0x200A) || r == 0x2028 || r == 0x2029 ||
			r == 0x202F || r == 0x205F || r == 0x3000 || r == 0xFEFF
	})
}

func lowerInputText(value string) string {
	// JS uses full lowercase mapping; Go's simple lowercase would turn İ into i
	// and incorrectly classify hİ as a greeting. Other expansions cannot match
	// these fixed lexical rules (including the contextual Greek sigma mapping).
	return strings.ToLower(strings.ReplaceAll(value, "İ", "i\u0307"))
}

// ClaimSDKInput is independent of response/control-plane input ownership.
func (r *Request) ClaimSDKInput() bool {
	if r == nil {
		return false
	}
	r.tracker.mu.Lock()
	defer r.tracker.mu.Unlock()
	defer r.tracker.saveSDKSessionLocked(r.call.state.scope)
	if !r.identity.StartsPrompt || r.call.state.sdkInputClaimed {
		return false
	}
	r.call.state.sdkInputClaimed = true
	return true
}
