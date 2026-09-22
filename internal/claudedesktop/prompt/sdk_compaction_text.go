package prompt

import "errors"

// SDKCompactionText is transient operation content. Its fields deliberately do
// not marshal to JSON; only Fingerprint may cross into long-lived accounting.
type SDKCompactionText struct {
	selected, normalized string
	known                bool
}

type SDKCompactionWrapOptions struct {
	TranscriptPath            string
	RecentMessagesPreserved   bool
	REPLStateCleared          bool
	SuppressFollowUpQuestions bool
}

func (s SDKCompactionText) SelectedText() string { return s.selected }

func (s SDKCompactionText) Fingerprint() SDKCompactionSummary {
	if !s.known {
		return SDKCompactionSummary{}
	}
	return SDKCompactionSummary{bytes: len(s.normalized), sha256: sdkCompactionHash(s.normalized), reviewed: true}
}

// Wrap follows the pinned Nhe options. It does not invent a transcript path,
// retained-message claim or REPL reset when that operation did not occur.
func (s SDKCompactionText) Wrap(options SDKCompactionWrapOptions) (string, error) {
	if !s.known {
		return "", errors.New("unobserved Desktop compaction summary")
	}
	text := "This session is being continued from a previous conversation that ran out of context. The summary below covers the earlier portion of the conversation.\n\n" + s.normalized
	if options.TranscriptPath != "" {
		text += "\n\nIf you need specific details from before compaction (like exact code snippets, error messages, or content you generated), read the full transcript at: " + options.TranscriptPath
	}
	if options.RecentMessagesPreserved {
		text += "\n\nRecent messages are preserved verbatim."
	}
	if options.REPLStateCleared {
		text += "\n\n" + `Your REPL VM state has been cleared as part of this compaction. Variables defined in REPL calls before this point are no longer accessible — redefine any you still need.`
	}
	if options.SuppressFollowUpQuestions {
		text += "\n" + `Continue the conversation from where it left off without asking the user any further questions. Resume directly — do not acknowledge the summary, do not recap what was happening, do not preface with "I'll continue" or similar. Pick up the last task as if the break never happened.`
	}
	return text, nil
}
