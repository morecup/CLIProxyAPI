package prompt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// SDKCompactionApplication is a request-local candidate, not an applied
// boundary. The executor must complete restoration/PostCompact and render its
// next main request before Commit. Neither the staging operation nor Commit
// emits a native lifecycle success event.
type SDKCompactionApplication struct {
	view         *SDKCompactionView
	rows         []json.RawMessage
	messages     []SDKHistoryMessage
	fingerprints []string
	summary      SDKCompactionSummary
	selectedHash string
	helperKey    string
	postTokens   int64
	restored     bool
	nativeRows   []SDKNativeMessage
}

// PrepareApplication retains exact observed rows and native UUIDs. Only an entire
// suffix selected by the reviewed group loop can be preserved. Unsupported
// restored attachments, hidden yields and unowned tool history require their own
// application contract; this method does not silently reconstruct them.
func (v *SDKCompactionView) PrepareApplication(text SDKCompactionText, options SDKCompactionWrapOptions, preserve []SDKHistoryMessage, helperCallID string) (*SDKCompactionApplication, error) {
	if !v.Current() {
		return nil, ErrSDKCompactionViewStale
	}
	if !text.known || len(preserve) == 0 || len(preserve) >= len(v.history.Messages) {
		return nil, ErrSDKCompactionContentUnknown
	}
	start := len(v.history.Messages) - len(preserve)
	groupStart, wholeGroup := 0, false
	for _, group := range GroupSDKHistory(v.history.Messages) {
		if groupStart == start {
			wholeGroup = true
			break
		}
		groupStart += len(group)
	}
	if !wholeGroup || helperCallID == "" {
		return nil, ErrSDKCompactionContentUnknown
	}
	for index, message := range preserve {
		if message != v.history.Messages[start+index] {
			return nil, ErrSDKCompactionContentUnknown
		}
	}
	rows, err := v.Resolve(preserve)
	if err != nil {
		return nil, err
	}
	wrapped, err := text.Wrap(options)
	if err != nil {
		return nil, err
	}
	content, err := json.Marshal(wrapped)
	if err != nil {
		return nil, err
	}
	summaryRow := append(append([]byte(`{"role":"user","content":`), content...), '}')
	application := &SDKCompactionApplication{view: v, rows: append([]json.RawMessage{summaryRow}, rows...), summary: text.Fingerprint(), helperKey: digest("compact", helperCallID)}
	application.selectedHash = sdkCompactionHash(text.SelectedText())
	application.messages = []SDKHistoryMessage{
		{Type: "system", Subtype: "compact_boundary", UUID: uuid.NewString()},
		{Type: "user", UUID: uuid.NewString(), TokenEstimate: EstimateSDKContent(content)},
	}
	createdAt := nativeContentTimestamp(time.Now())
	application.nativeRows = []SDKNativeMessage{
		{Type: "system", Subtype: "compact_boundary", UUID: application.messages[0].UUID, Timestamp: createdAt},
		{Type: "user", UUID: application.messages[1].UUID, Timestamp: createdAt, IsCompactSummary: true, Message: summaryRow},
	}
	for _, message := range preserve {
		// Native t1e retains identity/content and zeroes the four usage fields,
		// so a precompact response cannot anchor the compacted context size.
		if message.Type == "assistant" {
			message.Usage = SDKTokenUsage{}
			message.UsageKnown = true
		}
		application.messages = append(application.messages, message)
	}
	for _, message := range application.messages {
		if message.Type == "user" || message.Type == "assistant" || message.Type == "attachment" {
			if !message.TokenEstimate.Known {
				return nil, ErrSDKReactiveEstimateUnknown
			}
			application.postTokens += message.TokenEstimate.Tokens
		}
	}
	body, err := json.Marshal(struct {
		Messages []json.RawMessage `json:"messages"`
	}{application.rows})
	if err != nil {
		return nil, ErrSDKCompactionContentUnknown
	}
	var known bool
	application.fingerprints, known = sdkWireRequestFingerprints(body)
	if !known {
		return nil, ErrSDKCompactionContentUnknown
	}
	return application, nil
}

func (a *SDKCompactionApplication) Messages() []json.RawMessage {
	if a == nil {
		return nil
	}
	rows := make([]json.RawMessage, len(a.rows))
	for index, row := range a.rows {
		rows[index] = append(json.RawMessage(nil), row...)
	}
	return rows
}

// PostTokens uses native wp (content sum), not the usage-anchored kp estimate.
func (a *SDKCompactionApplication) PostTokens() int64 {
	if a == nil {
		return 0
	}
	return a.postTokens
}

// Commit atomically changes both native history and active query ownership.
// It accepts a fully rendered next main request in the same scope, with a new
// physical request ID. A late callback from the precompact request can neither
// settle this new call nor append old response objects after the boundary.
func (a *SDKCompactionApplication) Commit(ctx context.Context, input Input) (*Request, error) {
	if ctx == nil {
		return nil, errors.New("compaction application requires its cancellation context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a == nil || a.view == nil || a.view.owner == nil {
		return nil, ErrSDKCompactionViewStale
	}
	view, previous := a.view, a.view.owner
	if input.Role != "main" || input.PromptID != previous.Identity().PromptID || digest(input.AccountID, input.SessionID) != previous.call.state.scope || input.Attempt > 1 {
		return nil, ErrSDKCompactionViewStale
	}
	clientID, err := uuid.Parse(input.ClientRequestID)
	if err != nil || clientID == uuid.Nil || len(input.Body) > maxSDKCompactionViewBytes {
		return nil, ErrSDKCompactionContentUnknown
	}
	actual, known := sdkWireRequestFingerprints(input.Body)
	if !known || len(actual) != len(a.fingerprints) {
		return nil, ErrSDKCompactionContentUnknown
	}
	for index, expected := range a.fingerprints {
		if actual[index] != expected {
			return nil, ErrSDKCompactionContentUnknown
		}
	}
	previous.tracker.mu.Lock()
	defer previous.tracker.mu.Unlock()
	defer previous.tracker.saveSDKSessionLocked(previous.call.state.scope)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !view.currentLocked() {
		return nil, ErrSDKCompactionViewStale
	}
	s := previous.call.state
	requestKey := digest(input.ClientRequestID)
	if s.requests[requestKey] != nil || len(s.requests) >= maxRequestsPerPrompt {
		return nil, errors.New("compaction continuation requires an unused request identity")
	}
	returned := make(map[string]bool, len(previous.call.resultIDs))
	for _, id := range previous.call.resultIDs {
		returned[id] = true
	}
	for id := range s.pending {
		if !returned[id] {
			return nil, ErrSDKCompactionContentUnknown
		}
	}
	if s.sdk.result != nil {
		return nil, ErrSDKCompactionContentUnknown
	}
	operation := s.sdk.compactions[a.helperKey]
	if operation == nil || operation.applied || operation.discarded || !operation.observation.input.known || operation.observation.summary != a.summary {
		return nil, ErrSDKCompactionContentUnknown
	}
	if input.StartedAt.IsZero() {
		input.StartedAt = time.Now()
	}
	identity := s.identity
	identity.StartsPrompt = false
	identity.QueryDepth = len(s.requests)
	sum := sha256.Sum256(input.Body)
	next := &call{state: s, identity: identity, inputDigest: requestIdentityDigest(input.Body), startedAt: input.StartedAt, attempt: 1,
		resultIDs:        append([]string(nil), previous.call.resultIDs...),
		sdkQueryObserved: true, sdkWireInputKnown: true, sdkWireInputs: actual, sdkWireRequestDigest: hex.EncodeToString(sum[:])}
	// All validation is complete before mutating any shared state.
	previous.finished, previous.call.settled = true, true
	if previous.call.reactiveCompactionFailure {
		s.sdk.recoveredAPIFailures++
	}
	s.sdk.history.messages = append([]SDKHistoryMessage(nil), a.messages...)
	previous.tracker.commitNativeCompactionLocked(s.scope, a, input.PromptID)
	s.sdk.history.expectedText, s.sdk.history.expectedTextKnown = nil, false
	s.sdk.numTurns++
	s.sdk.compactionSummaryUserYields++
	s.sdk.sawCompact = true
	s.sdk.tools = nil
	s.sdk.queries++
	operation.applied = true
	s.sdk.pendingCompactions--
	s.requests[requestKey], s.active, s.lastAt = next, next, input.StartedAt
	return &Request{tracker: previous.tracker, call: next, identity: identity, attempt: 1}, nil
}

func (a *SDKCompactionApplication) Discard() {
	if a != nil {
		if a.view != nil {
			a.view.discardHelperKey(a.helperKey)
		}
		*a = SDKCompactionApplication{}
	}
}
