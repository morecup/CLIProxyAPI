package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/google/uuid"
)

// Native SendMessage tool contract (Claude Code 2.1.247): input validation
// texts, the in-process delivery lanes, ordered result objects, the
// per-conversation pin guard and the tool_result block mapping.

const (
	toMaxLength      = 300
	summaryMaxLength = 200
	resumeDisplayMax = 7
)

const (
	textBroadcastUnsupported = "broadcast (to: \"*\") is no longer supported \u2014 send a message per recipient"
	textBareNameRequired     = "to must be a bare teammate name \u2014 there is only one team per session"
	textMessageEmpty         = "message must not be empty"
	textProtocolFrame        = "message text must not be a teammate protocol frame (permission/mode/plan/shutdown JSON) \u2014 to respond to a plan or shutdown request, use the structured object form ({\"message\": {\"type\": ...}}); otherwise send plain text"
	textLifecycleFrame       = "message text must not be a teammate lifecycle/task frame (idle/terminated/task/shutdown JSON) \u2014 send plain text instead"
	textToSingleLine         = "must be a single-line recipient name or address"
	textToTooLong            = "recipient longer than any listed name or address (max 300 characters)"
	textQueuedMain           = "Message queued for the main conversation's next turn."
	textMainSelf             = "You are the main conversation \u2014 \"main\" addresses you. Send to a named agent instead."
	textResumedReportAbsent  = "Resumed agent. Its final report is not in this message."
	textResumedReportFollows = "Resumed agent. Its final report follows this JSON, framed by the harness."
	textResumedWithheld      = "Resumed agent. Its final report was withheld: a hook rewrote this result and dropped the framed hand-back."
	textNotFoundHint         = "Check the spelling, or use the agent ID from a background agent's spawn result."
)

var protocolFrameTypes = []string{"permission_request", "permission_response", "sandbox_permission_request", "sandbox_permission_response", "shutdown_request", "shutdown_approved", "team_permission_update", "mode_set_request", "plan_approval_request", "plan_approval_response"}
var lifecycleFrameTypes = []string{"idle_notification", "teammate_terminated", "task_assignment", "task_completed", "shutdown_rejected"}

// pendingMessage is one queued delivery awaiting the child's next tool round.
// Records from earlier local builds stored bare strings; they load as
// coordinator text so persisted queues stay deliverable.
type pendingMessage struct {
	Text   string        `json:"text"`
	Origin messageOrigin `json:"origin"`
	IsMeta bool          `json:"isMeta"`
}

func (p *pendingMessage) UnmarshalJSON(data []byte) error {
	var text string
	if json.Unmarshal(data, &text) == nil {
		*p = pendingMessage{Text: text, Origin: coordinatorOrigin, IsMeta: true}
		return nil
	}
	type plain pendingMessage
	var value plain
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*p = pendingMessage(value)
	return nil
}

// sendResult is the native SendMessage data object. Fields keep the literal
// order of the native lanes; omitted fields never serialize.
type sendResult struct {
	Success        bool     `json:"success"`
	Message        string   `json:"message"`
	Display        string   `json:"display,omitempty"`
	ResumedAgentID string   `json:"resumedAgentId,omitempty"`
	Pin            *sendPin `json:"pin,omitempty"`
}

func (s sendResult) raw() (json.RawMessage, error) {
	raw := jsonStringify(s)
	if raw == nil {
		return nil, ErrInvalid
	}
	return raw, nil
}

func failure(message string) (json.RawMessage, error) {
	return sendResult{Success: false, Message: message}.raw()
}

func failureWithDisplay(message, display string) (json.RawMessage, error) {
	return sendResult{Success: false, Message: message, Display: display}.raw()
}

// frameType reports the "type" of a JSON object message when it names a
// teammate protocol or lifecycle frame.
func frameType(message string) string {
	var value struct {
		Type json.RawMessage `json:"type"`
	}
	trimmed := strings.TrimFunc(message, jsTrimSpace)
	if !strings.HasPrefix(trimmed, "{") || json.Unmarshal([]byte(message), &value) != nil {
		return ""
	}
	var kind string
	if json.Unmarshal(value.Type, &kind) != nil {
		return ""
	}
	return kind
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// validateSendMessage applies the native schema and validateInput order for
// the in-process recipients this runtime serves. Structured messages and
// teammate/socket addressing are not established locally.
func validateSendMessage(input json.RawMessage) (to, message string, err error) {
	var value struct {
		To      *string         `json:"to"`
		Message json.RawMessage `json:"message"`
		Summary *string         `json:"summary"`
	}
	if json.Unmarshal(input, &value) != nil || value.To == nil {
		return "", "", errors.New("SendMessage input requires a string recipient")
	}
	to = *value.To
	if strings.ContainsAny(to, "\r\n\u2028\u2029") {
		return "", "", errors.New(textToSingleLine)
	}
	if utf16Length(to) > toMaxLength {
		return "", "", errors.New(textToTooLong)
	}
	if value.Summary != nil && utf16Length(*value.Summary) > summaryMaxLength {
		return "", "", errors.New("summary must be at most 200 characters")
	}
	if len(value.Message) != 0 && json.Unmarshal(value.Message, &message) != nil {
		return "", "", errors.New("structured SendMessage messages are not available in this query")
	}
	if to == "*" {
		return "", "", errors.New(textBroadcastUnsupported)
	}
	if strings.Contains(to, "@") {
		return "", "", errors.New(textBareNameRequired)
	}
	if strings.TrimFunc(message, jsTrimSpace) == "" {
		return "", "", errors.New(textMessageEmpty)
	}
	if kind := frameType(message); kind != "" {
		if containsString(protocolFrameTypes, kind) {
			return "", "", errors.New(textProtocolFrame)
		}
		if containsString(lifecycleFrameTypes, kind) {
			return "", "", errors.New(textLifecycleFrame)
		}
	}
	return to, message, nil
}

// senderIdentity is the native sender label: the caller task's name, else
// its agent ID. The main conversation has no peer identity.
func (r *Runtime) senderIdentityLocked(caller Caller) string {
	if caller.AgentID == "" {
		return ""
	}
	if t := r.tasks[caller.AgentID]; t != nil && t.Name != "" {
		return t.Name
	}
	return caller.AgentID
}

// candidatesLocked mirrors the native listing: main first, then registered
// names in insertion order with their task start times.
func (r *Runtime) candidatesLocked() []addressCandidate {
	list := []addressCandidate{{Name: mainAgentName, ID: r.options.MainAgentID, Kind: mainAgentName}}
	for _, name := range r.nameOrder {
		id := r.names[name]
		c := addressCandidate{Name: name, ID: id, Kind: "subagent"}
		if t := r.tasks[id]; t != nil {
			c.LastActive, c.HasActive = t.StartedAt, true
		}
		list = append(list, c)
	}
	assignRefs(list)
	return list
}

// resolveRecipientLocked follows the native lanes for in-process agents.
func (r *Runtime) resolveRecipientLocked(to string) (resolution, *addressBook) {
	book := newAddressBook(r.candidatesLocked())
	for _, scheme := range []string{"uds:", "bridge:", "did:"} {
		if strings.HasPrefix(to, scheme) {
			return resolution{Kind: "not-found"}, book
		}
	}
	if to == mainAgentName {
		return resolution{Kind: mainAgentName}, book
	}
	if id := r.names[to]; id != "" {
		return resolution{Kind: "agent", ID: id, Name: to}, book
	}
	if isAgentID(to) {
		return resolution{Kind: "agent", ID: to, Name: to}, book
	}
	if _, _, ok := parseNameRef(to); !ok {
		normalized := normalizeAgentName(to)
		for _, name := range r.nameOrder {
			if normalizeAgentName(name) == normalized {
				return resolution{Kind: "agent", ID: r.names[name], Name: name}, book
			}
		}
	}
	return book.resolveAddress(to), book
}

func quoteCandidates(list []addressCandidate) string {
	parts := make([]string, 0, len(list))
	for _, c := range list {
		parts = append(parts, "'"+c.Name+"' in this session")
	}
	return strings.Join(parts, ", ")
}

func notFoundResult(to string, closest []addressCandidate) (json.RawMessage, error) {
	if len(closest) == 0 {
		return failureWithDisplay("No agent named '"+to+"' is reachable.\n"+textNotFoundHint,
			"Not sent \u2014 no agent named '"+to+"' is reachable.")
	}
	addresses, names := make([]string, 0, len(closest)), make([]string, 0, len(closest))
	for _, c := range closest {
		addresses = append(addresses, candidateAddress(c))
		names = append(names, c.Name)
	}
	return failureWithDisplay("No agent named '"+to+"' is reachable. Did you mean: "+strings.Join(addresses, ", ")+"?\n"+textNotFoundHint,
		"Not sent \u2014 no agent named '"+to+"' is reachable. Did you mean: "+strings.Join(names, ", ")+"?")
}

func ambiguousResult(to string, candidates []addressCandidate, now time.Time) (json.RawMessage, error) {
	count := strconv.Itoa(len(candidates))
	lines := make([]string, 0, len(candidates))
	for _, c := range candidates {
		lines = append(lines, "  "+describeCandidate(c, now))
	}
	return failureWithDisplay("'"+to+"' matches "+count+" agents by prefix. Re-send with the ref of the one you mean:\n"+strings.Join(lines, "\n"),
		"Not sent \u2014 '"+to+"' matches "+count+" agents by prefix ("+quoteCandidates(candidates)+"); asked Claude to pick one.")
}

func reboundResult(name string, previous sendPin, next *addressCandidate, now time.Time) (json.RawMessage, error) {
	message := "'" + name + "' now resolves to a different agent than it did earlier in this conversation: earlier sends went to [" + previous.Ref + "], which this name no longer reaches. Nothing was sent.\n"
	if next != nil {
		message += "It now resolves to:\n  " + describeCandidate(*next, now) + "\nTo message the new agent, re-send with its ref:\ne.g. {\"to\": \"" + candidateAddress(*next) + "\", ...}\nIf you need the earlier agent and it is still running, address it by its agent ID from its spawn result."
	} else {
		message += textNotFoundHint
	}
	return failureWithDisplay(message, "Not sent \u2014 '"+name+"' now means a different agent than it did earlier in this conversation; asked Claude to confirm which one it wants.")
}

// resumeDisplayName shortens a native agent ID to seven UTF-16 units for the
// "Resuming agent" text; names pass through.
func resumeDisplayName(name string) string {
	if !nativeAgentIDPattern.MatchString(name) {
		return name
	}
	units := utf16.Encode([]rune(name))
	if len(units) <= resumeDisplayMax {
		return name
	}
	return string(utf16.Decode(units[:resumeDisplayMax]))
}

// sendMessage executes the tool after validation. The runtime lock is held
// across resolution, the pin guard and the lane so admission stays atomic
// with Stop and concurrent senders.
func (r *Runtime) sendMessage(ctx context.Context, caller Caller, input json.RawMessage) (json.RawMessage, error) {
	to, message, err := validateSendMessage(input)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if r.closed || r.ctx.Err() != nil || ctx.Err() != nil || !validPromptID(caller.PromptID) {
		r.mu.Unlock()
		return nil, ErrUnavailable
	}
	now := r.options.Now()
	sender := r.senderIdentityLocked(caller)
	resolved, book := r.resolveRecipientLocked(to)
	switch resolved.Kind {
	case "not-found":
		r.mu.Unlock()
		return notFoundResult(to, resolved.Closest)
	case "ambiguous":
		r.mu.Unlock()
		return ambiguousResult(to, resolved.Candidates, now)
	}
	var kind, status string
	var target *task
	if resolved.Kind == "agent" {
		target = r.tasks[resolved.ID]
		switch {
		case target == nil:
			kind = "evicted"
		case target.Status == "running" || target.resuming:
			kind = "live"
		case target.StoppedByUser:
			kind = "stopped-by-user"
		default:
			kind, status = "stopped", target.Status
		}
	}
	decision := decidePin(r.pins, to, resolved, kind, book)
	if decision.Rebound {
		r.mu.Unlock()
		return reboundResult(resolved.Name, decision.Previous, decision.Next, now)
	}
	if decision.SetPin {
		if r.pins == nil {
			r.pins = map[string]sendPin{}
		}
		r.pins[decision.Key] = *decision.Pin
		if err := r.saveLocked(); err != nil {
			delete(r.pins, decision.Key)
			r.mu.Unlock()
			return nil, err
		}
	}
	text, origin := message, coordinatorOrigin
	if sender != "" {
		text, origin = peerDelivery(sender, caller.AgentID, message)
	}
	if resolved.Kind == mainAgentName {
		r.mu.Unlock()
		if sender == "" {
			return failure(textMainSelf)
		}
		if r.options.MessageMain == nil {
			return nil, ErrUnavailable
		}
		if err := r.options.MessageMain(MainDelivery{Text: text, Origin: jsonStringify(origin)}); err != nil {
			return nil, err
		}
		return sendResult{Success: true, Message: textQueuedMain}.raw()
	}
	switch kind {
	case "evicted":
		r.mu.Unlock()
		return failure("Agent \"" + resolved.Name + "\" could not be resumed: no task with this agent ID is recorded in this session")
	case "stopped-by-user":
		r.mu.Unlock()
		return failure("Agent \"" + resolved.Name + "\" was stopped by the user and was not resumed. Treat its work as cancelled; only start a new agent for it if the user explicitly asks.")
	case "live":
		target.Pending = append(target.Pending, pendingMessage{Text: text, Origin: origin, IsMeta: true})
		err := r.saveLocked()
		if err != nil {
			target.Pending = target.Pending[:len(target.Pending)-1]
			target.persistenceFailed = true
		}
		r.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return sendResult{Success: true, Message: "Message queued for delivery to " + resolved.Name + " at its next tool round.", Pin: decision.Pin}.raw()
	}
	if target.active != nil {
		r.mu.Unlock()
		return failure("Agent \"" + resolved.Name + "\" is stopped (" + status + ") and could not be resumed: its previous execution has not exited")
	}
	// Admission is atomic with Stop and other SendMessage callers. Native
	// metadata/forked-skill restoration must be validated before extending
	// this store to histories not produced by this runtime.
	running := 0
	for _, current := range r.tasks {
		if current.active != nil || current.Status == "running" || current.resuming {
			running++
		}
	}
	if running >= r.options.MaxConcurrent {
		r.mu.Unlock()
		return failure("Concurrent subagent limit reached")
	}
	target.resuming = true
	previous := target.record
	prompt := midTurnProjection(text, origin)
	target.Messages = append(cloneMessages(target.Messages), textMessage(prompt))
	target.Pending = nil
	target.Status, target.Background, target.Kind = "running", true, "resume"
	target.ParentAgentID, target.ParentPromptID, target.PromptID = caller.AgentID, caller.PromptID, uuid.NewString()
	target.Generation++
	target.StartedAt, target.FinishedAt = now, time.Time{}
	target.Result, target.Tokens, target.ToolUses, target.UsageKnown = nil, 0, 0, true
	target.ResultSections = resultSections{}
	target.Error, target.KilledBy, target.Notified = "", "", false
	r.enqueueEventLocked(target, "started", false)
	err = r.saveLocked()
	target.resuming = false
	if err != nil {
		r.outbox = r.outbox[:len(r.outbox)-1]
		target.record = previous
		target.persistenceFailed = true
		r.mu.Unlock()
		return nil, err
	}
	r.appendTranscriptMetaLocked(target, target.Messages[len(target.Messages)-1].Content, origin, uuid.NewString(), target.StartedAt)
	if err = r.saveLocked(); err != nil {
		target.persistenceFailed = true
	}
	r.startLocked(ctx, target)
	id := target.ID
	r.mu.Unlock()
	return sendResult{Success: true, Message: "Resuming agent " + resumeDisplayName(resolved.Name), ResumedAgentID: id, Pin: decision.Pin}.raw()
}

// pinDecision is the outcome of the native pin guard for one send.
type pinDecision struct {
	Rebound  bool
	Pin      *sendPin
	SetPin   bool
	Key      string
	Previous sendPin
	Next     *addressCandidate
}

// decidePin follows the native guard: main, not-found, ambiguous and
// stopped-by-user recipients carry no pin; a pinned spelling that now reaches
// another agent is rebound unless the caller typed a differing exact spelling
// or an explicit "name [ref]" address.
func decidePin(pins map[string]sendPin, to string, resolved resolution, kind string, book *addressBook) pinDecision {
	if resolved.Kind != "agent" || kind == "stopped-by-user" {
		return pinDecision{}
	}
	key := normalizeAgentName(resolved.Name)
	previous, pinned := pins[key]
	if pinned && previous.ID == resolved.ID {
		return pinDecision{Pin: &previous}
	}
	if pinned {
		_, _, explicit := parseNameRef(to)
		if !explicit && to == resolved.Name && to != previous.Name {
			return pinDecision{}
		}
		if !explicit {
			decision := pinDecision{Rebound: true, Previous: previous}
			if c, ok := book.one(key, ""); ok && c.ID == resolved.ID {
				decision.Next = &c
			}
			return decision
		}
	}
	pin := sendPin{ID: resolved.ID, Name: resolved.Name, Ref: candidateHash("subagent", resolved.ID)[:refLength]}
	return pinDecision{Pin: &pin, SetPin: true, Key: key}
}

// consumePendingLocked moves queued deliveries into the child's next request
// as system-reminder wrapped mid-turn projections and records the native
// queued_command attachment plus the meta user row in its transcript.
func (r *Runtime) consumePendingLocked(t *task, at time.Time) bool {
	if len(t.Pending) == 0 {
		return false
	}
	for _, entry := range t.Pending {
		sourceUUID := uuid.NewString()
		attachment := jsonStringify(struct {
			Type       string        `json:"type"`
			Prompt     string        `json:"prompt"`
			SourceUUID string        `json:"source_uuid"`
			Origin     messageOrigin `json:"origin"`
			IsMeta     bool          `json:"isMeta"`
		}{"queued_command", entry.Text, sourceUUID, entry.Origin, entry.IsMeta})
		r.appendTranscriptAttachmentLocked(t, attachment, at)
		t.Messages = append(t.Messages, textMessage(queuedDeliveryWire(entry.Text, entry.Origin)))
		r.appendTranscriptMetaLocked(t, t.Messages[len(t.Messages)-1].Content, entry.Origin, sourceUUID, at)
	}
	t.Pending = nil
	return true
}

// MainDelivery is a SendMessage addressed to the main conversation: the
// wrapped text and its native origin, delivered as a meta input.
type MainDelivery struct {
	Text   string
	Origin json.RawMessage
}

// FreshTurnWire returns the wire content of a queued main-conversation
// delivery when it starts a fresh turn.
func FreshTurnWire(delivery MainDelivery) string {
	var origin messageOrigin
	_ = json.Unmarshal(delivery.Origin, &origin)
	return freshTurnProjection(delivery.Text, origin)
}

// stripTopLevelKeys removes object members while preserving the remaining
// members' bytes and order, as the native {...rest} spread does.
func stripTopLevelKeys(raw json.RawMessage, drop ...string) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("SendMessage output is not an object")
	}
	var out bytes.Buffer
	out.WriteByte('{')
	first := true
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, _ := keyToken.(string)
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		if containsString(drop, key) {
			continue
		}
		if !first {
			out.WriteByte(',')
		}
		first = false
		out.Write(jsonStringify(key))
		out.WriteByte(':')
		out.Write(value)
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

// sendMessageBlock is the native mapToolResultToToolResultBlockParam for
// SendMessage: one text block holding the JSON without display and hand-back
// fields, plus the framed hand-back when provenance or a skipped review
// requires it. provenance is read only when an inline hand-back is present,
// as native does; frame renders that hand-back. The runtime never produces
// one because local resumes run in the background.
func sendMessageBlock(toolUseID string, data json.RawMessage, provenance func() bool, frame func(json.RawMessage) (string, error)) (json.RawMessage, error) {
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(data) {
		return nil, errors.New("SendMessage output is not an object")
	}
	var fields struct {
		InlineHandback       json.RawMessage `json:"inlineHandback"`
		HandoffReviewSkipped bool            `json:"handoffReviewSkipped"`
		Display              json.RawMessage `json:"display"`
	}
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	rest, err := stripTopLevelKeys(data, "display", "inlineHandback", "handoffReviewSkipped")
	if err != nil {
		return nil, err
	}
	text := string(rest)
	hasHandback := len(fields.InlineHandback) != 0 && string(fields.InlineHandback) != "null"
	fixed := func(message string) string {
		return string(jsonStringify(sendResult{Success: true, Message: message}))
	}
	switch {
	case hasHandback && (provenance() || fields.HandoffReviewSkipped):
		framed, err := frame(fields.InlineHandback)
		if err != nil {
			return nil, err
		}
		text = fixed(textResumedReportFollows) + "\n" + framed
	case hasHandback:
		var handback struct {
			DisplayName string       `json:"displayName"`
			Content     []resultText `json:"content"`
		}
		if err := json.Unmarshal(fields.InlineHandback, &handback); err != nil {
			return nil, err
		}
		var parts []string
		for _, block := range handback.Content {
			if block.Type == "text" {
				parts = append(parts, block.Text)
			}
		}
		body := strings.Join(parts, "\n")
		if body == "" {
			body = "(no text output)"
		}
		rest, err = replaceMessage(rest, "Resumed agent "+handback.DisplayName+". Result:\n\n"+body)
		if err != nil {
			return nil, err
		}
		text = string(rest)
	case fields.HandoffReviewSkipped:
		text = fixed(textResumedWithheld)
	}
	block := jsonStringify(struct {
		ToolUseID string       `json:"tool_use_id"`
		Type      string       `json:"type"`
		Content   []resultText `json:"content"`
	}{toolUseID, "tool_result", []resultText{{Type: "text", Text: text}}})
	if block == nil {
		return nil, ErrInvalid
	}
	return block, nil
}

// replaceMessage rewrites the "message" member in place, keeping every other
// member's bytes and position.
func replaceMessage(raw json.RawMessage, message string) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, errors.New("SendMessage output is not an object")
	}
	var out bytes.Buffer
	out.WriteByte('{')
	first := true
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, _ := keyToken.(string)
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		if key == "message" {
			value = jsonStringify(message)
		}
		if !first {
			out.WriteByte(',')
		}
		first = false
		out.Write(jsonStringify(key))
		out.WriteByte(':')
		out.Write(value)
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

// frameHandback renders an inline hand-back with the harness framing used by
// completed Agent results.
func frameHandback(raw json.RawMessage) (string, error) {
	var handback struct {
		Content            []resultText `json:"content"`
		HarnessNoteCount   float64      `json:"harnessNoteCount"`
		HarnessTailCount   float64      `json:"harnessTailCount"`
		HarnessSectionHash string       `json:"harnessSectionHash"`
	}
	if err := json.Unmarshal(raw, &handback); err != nil {
		return "", err
	}
	return frameResult(handback.Content, handback.HarnessNoteCount, handback.HarnessTailCount, handback.HarnessSectionHash)
}
