package tasks

import (
	"context"
	"encoding/json"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/transcript"
)

func (r *Runtime) appendTranscriptInputLocked(t *task, message Message, at time.Time) {
	if t.transcript == nil || t.TranscriptFailed {
		return
	}
	if message.Role != "user" || t.transcript.AppendInput(message.Content, t.PromptID, at) != nil {
		t.TranscriptFailed = true
	}
	t.TranscriptLeaf = t.transcript.Leaf()
}

// appendTranscriptMetaLocked records a SendMessage delivery as the native
// isMeta user row carrying its origin under the attachment's source UUID.
func (r *Runtime) appendTranscriptMetaLocked(t *task, content json.RawMessage, origin messageOrigin, uuid string, at time.Time) {
	if t.transcript == nil || t.TranscriptFailed {
		return
	}
	if t.transcript.AppendMetaInput(content, jsonStringify(origin), uuid, t.PromptID, at) != nil {
		t.TranscriptFailed = true
	}
	t.TranscriptLeaf = t.transcript.Leaf()
}

func (r *Runtime) appendTranscriptAttachmentLocked(t *task, attachment json.RawMessage, at time.Time) {
	if t.transcript == nil || t.TranscriptFailed {
		return
	}
	if t.transcript.AppendAttachment(attachment, at) != nil {
		t.TranscriptFailed = true
	}
	t.TranscriptLeaf = t.transcript.Leaf()
}

func (r *Runtime) nativeObserverFactory(ctx context.Context, id string, generation uint64, run *execution, invocation uint64) func() func([]transcript.Message, string) error {
	return func() func([]transcript.Message, string) error {
		r.mu.Lock()
		t := r.tasks[id]
		if r.closed || ctx.Err() != nil || t == nil || t.active != run || t.Generation != generation || t.nativeInvocation != invocation || t.Status != "running" {
			r.mu.Unlock()
			return func([]transcript.Message, string) error { return ErrNativeObserverRetired }
		}
		t.nativeAttempt++
		attempt := t.nativeAttempt
		r.mu.Unlock()
		return func(rows []transcript.Message, issue string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			t := r.tasks[id]
			if r.closed || ctx.Err() != nil || t == nil || t.active != run || t.Generation != generation || t.nativeInvocation != invocation || t.nativeAttempt != attempt || t.Status != "running" {
				return ErrNativeObserverRetired
			}
			if t.transcript == nil || t.TranscriptFailed {
				return ErrUnavailable
			}
			err := t.transcript.Observe(rows, issue)
			t.lastRequestID = lastAssistantRequestID(rows, t.lastRequestID)
			t.TranscriptLeaf = t.transcript.Leaf()
			if err != nil {
				t.TranscriptFailed = true
			}
			if saveErr := r.saveLocked(); saveErr != nil {
				t.persistenceFailed = true
				return saveErr
			}
			return err
		}
	}
}

func (r *Runtime) outputPath(id string) string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.tasks[id]
	if t == nil || t.transcript == nil || t.TranscriptFailed {
		return ""
	}
	// Explicit retrieval drains real queued local writes before publishing the
	// complete output path. It is not an upstream network deadline.
	if t.transcript.Flush() != nil {
		t.TranscriptFailed = true
		return ""
	}
	return t.transcript.Path()
}
