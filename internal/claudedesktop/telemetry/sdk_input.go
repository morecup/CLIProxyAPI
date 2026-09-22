package telemetry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
)

const FactSDKInput = "input_prompt"

// A journal is independent of the prompt tracker's TTL. RunID deliberately
// prevents an application restart from claiming native resume semantics that
// have not been established. Such a session stays incomplete, not reset to 1.
// New session identities establish a new journal; an unknown slash/meta/clear
// history cannot silently restore a previously known counter. Headless SDK
// interruption neither changes this counter nor writes interrupted_message_id;
// the interactive CLI cancellation writer is a different execution path.
type sdkInputJournal struct {
	Version     int     `json:"version"`
	Binding     Binding `json:"binding"`
	SessionHash string  `json:"session_hash"`
	RunID       string  `json:"run_id"`
	Index       int64   `json:"index"`
	Known       bool    `json:"known"`
}

func (w *accountWorker) inputJournalPath(session string) string {
	return filepath.Join(w.sessionDir, sha256String(session)+".sdk-input")
}

func (w *accountWorker) readInputJournal(session string) (sdkInputJournal, error) {
	file := w.inputJournalPath(session)
	info, errStat := os.Stat(file)
	if errStat != nil {
		return sdkInputJournal{}, errStat
	}
	if info.Size() > 16*1024 {
		return sdkInputJournal{}, fmt.Errorf("SDK input journal exceeds limit")
	}
	encoded, errRead := readProtectedFile(file)
	if errRead != nil {
		return sdkInputJournal{}, fmt.Errorf("SDK input journal cannot be read")
	}
	var record sdkInputJournal
	if json.Unmarshal(encoded, &record) != nil || record.Version != 1 || record.Binding != w.binding || record.SessionHash != sha256String(session) || record.RunID == "" || record.Index < 0 || record.Index >= 1<<53 {
		return sdkInputJournal{}, fmt.Errorf("invalid SDK input journal; original retained")
	}
	return record, nil
}

func (w *accountWorker) writeInputJournal(session string, record sdkInputJournal) error {
	encoded, errEncode := json.Marshal(record)
	if errEncode != nil {
		return fmt.Errorf("SDK input journal cannot be encoded")
	}
	if errWrite := writeProtectedFile(w.inputJournalPath(session), encoded); errWrite != nil {
		return fmt.Errorf("SDK input journal cannot be persisted")
	}
	return nil
}

func (w *accountWorker) nextInputIndex(facts RequestFacts, firstSession bool) (int64, bool, error) {
	w.sessionMu.Lock()
	defer w.sessionMu.Unlock()
	key := sha256String(facts.SessionID)
	if w.inputJournalOverflow || w.inputJournalTainted[key] {
		return 0, false, fmt.Errorf("SDK input journal has an unresolved index persistence failure")
	}
	record, errRead := w.readInputJournal(facts.SessionID)
	if errors.Is(errRead, os.ErrNotExist) {
		record = sdkInputJournal{Version: 1, Binding: w.binding, SessionHash: key, RunID: w.manager.appSessionID,
			Known: firstSession && facts.Input.FreshConversation && facts.Attempt == 1}
	} else if errRead != nil {
		w.taintInputJournalLocked(key)
		return 0, false, errRead
	}
	if record.RunID != w.manager.appSessionID || !facts.Input.Known || facts.Attempt != 1 || record.Index >= (1<<53)-1 {
		record.Known = false
	}
	if record.Known {
		record.Index++
	}
	if errWrite := w.writeInputJournal(facts.SessionID, record); errWrite != nil {
		w.taintInputJournalLocked(key)
		return 0, false, errWrite
	}
	return record.Index, record.Known, nil
}

func (w *accountWorker) taintInputJournalLocked(key string) {
	if w.inputJournalTainted == nil {
		w.inputJournalTainted = make(map[string]bool)
	}
	if len(w.inputJournalTainted) >= maxPromptIssues {
		w.inputJournalOverflow = true
	} else {
		w.inputJournalTainted[key] = true
	}
}

func (s *RequestSpan) emitSDKInput(facts RequestFacts) {
	if s == nil || s.manager == nil || s.sdkWorker == nil || facts.Role != claudeprofile.RoleMain {
		return
	}
	if _, mapped := s.manager.sdkProfile.Events[FactSDKInput]; !mapped {
		return
	}
	if facts.Prompt == nil {
		s.retainResponseFactIssue(factIssueSDKInput, facts)
		return
	}
	if !facts.Prompt.ClaimSDKInput() {
		return
	}
	// A native isMeta input (a SendMessage delivery to the main conversation)
	// emits the event without prompt_index and leaves the index untouched.
	var index int64
	known, errIndex := true, error(nil)
	if !facts.Input.IsMeta {
		index, known, errIndex = s.sdkWorker.nextInputIndex(facts, s.firstTurn)
	}
	betas, modelKnown := s.manager.sdkProfile.InputBetaHeader(facts.Model)
	if errIndex != nil {
		s.sdkWorker.recordQueueFailure(errIndex)
	}
	if errIndex != nil || !known || !modelKnown || facts.Input.ObservedAt.IsZero() {
		s.retainResponseFactIssue(factIssueSDKInput, facts)
		return
	}
	metadata := map[string]any{
		"subscription_type": subscriptionType(s.sdkWorker.authSnapshot()), "cc_prompt_id": facts.PromptID,
		"is_negative": facts.Input.IsNegative, "is_keep_going": facts.Input.IsKeepGoing,
		"is_wakeup": facts.Input.IsWakeup, "prompt_length": facts.Input.Length,
		"prompt_source": "sdk",
	}
	if !facts.Input.IsMeta {
		metadata["prompt_index"] = index
	}
	if facts.EffortLevel != "" {
		metadata["effort_level"] = facts.EffortLevel
	}
	// The wrapper default is not the Messages header and must remain separate
	// even for Haiku, fast mode, credit fallback and cache-diagnosis requests.
	facts.Betas = betas
	if errEnqueue := s.manager.enqueueSDKEventAt(s.sdkWorker, FactSDKInput, facts, metadata, facts.Input.ObservedAt); errEnqueue != nil {
		s.sdkWorker.recordQueueFailure(errEnqueue)
		s.retainResponseFactIssue(factIssueSDKInput, facts)
	}
}
