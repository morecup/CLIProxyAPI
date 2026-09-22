package tasks

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

type sendMessageFixture struct {
	SDK       string `json:"sdk_sha256"`
	Constants struct {
		Tag                 string   `json:"tag"`
		RefLength           int      `json:"ref_length"`
		RefMaxLength        int      `json:"ref_max_length"`
		ClosestLimit        int      `json:"closest_limit"`
		NameMaxCodePoints   int      `json:"name_max_code_points"`
		AgentIDPattern      string   `json:"agent_id_pattern"`
		AddressPattern      string   `json:"address_pattern"`
		ToMaxLength         int      `json:"to_max_length"`
		SummaryMaxLength    int      `json:"summary_max_length"`
		QueuedMain          string   `json:"queued_main"`
		MainSelf            string   `json:"main_self"`
		ResumedAbsent       string   `json:"resumed_report_absent"`
		ResumedFollows      string   `json:"resumed_report_follows"`
		ResumedWithheld     string   `json:"resumed_report_withheld"`
		BroadcastUnsupport  string   `json:"broadcast_unsupported"`
		BareNameRequired    string   `json:"bare_name_required"`
		MessageEmpty        string   `json:"message_empty"`
		ProtocolFrame       string   `json:"protocol_frame"`
		LifecycleFrame      string   `json:"lifecycle_frame"`
		ProtocolTypes       []string `json:"protocol_types"`
		LifecycleTypes      []string `json:"lifecycle_types"`
		PeerPrefixFresh     string   `json:"peer_prefix_fresh"`
		PeerPrefixMidTurn   string   `json:"peer_prefix_mid_turn"`
		PeerTrailer         string   `json:"peer_trailer"`
		PeerMidTurnTail     string   `json:"peer_mid_turn_tail"`
		CoordinatorPrefix   string   `json:"coordinator_prefix"`
		NotFoundHintPresent bool     `json:"-"`
	} `json:"constants"`
	Regex struct {
		Open      struct{ Source, Flags string } `json:"open"`
		CloseOnly struct{ Source, Flags string } `json:"close_only"`
	} `json:"regex"`
	WrapperCases []struct {
		From, Message, Wrapped, Name, Body string
		BodyCloseOnly                      string `json:"body_close_only"`
		Origin                             json.RawMessage
	} `json:"wrapper_cases"`
	ProjectionCases []struct {
		Name, From, Message, Wrapped string
		FreshString                  string  `json:"fresh_string"`
		MidTurn                      string  `json:"mid_turn"`
		PendingWire                  string  `json:"pending_wire"`
		MidTurnIdempotent            *string `json:"mid_turn_idempotent"`
		Origin                       json.RawMessage
	} `json:"projection_cases"`
	PendingCases []struct {
		Name, Wrapped string
		Origin        json.RawMessage
		Entry         json.RawMessage
		Attachment    json.RawMessage
		Wire          []struct {
			Message struct{ Content string }
			IsMeta  bool
			UUID    string
			Origin  json.RawMessage
		}
	} `json:"pending_cases"`
	NormalizeCases []struct{ Input, Normalized string } `json:"normalize_cases"`
	ClosestCases   []struct {
		Query   string
		Names   []string
		Closest []string
	} `json:"closest_cases"`
	RefCases []struct {
		Candidates []struct{ Kind, ID string }
		Refs       []string
		Hashes     []string
	} `json:"ref_cases"`
	DurationCases []struct {
		MS   int64 `json:"ms"`
		Text string
	} `json:"duration_cases"`
	NotFoundCases []struct {
		To         string
		Result     json.RawMessage
		Candidates []fixtureCandidate
	} `json:"not_found_cases"`
	ResolveCases []struct {
		To, Normalized, Lane string
		Resolved             struct {
			Kind, AgentID, AgentName string
			Candidates               []fixtureCandidate
			Closest                  []fixtureCandidate
		}
		Result  json.RawMessage
		ClockMS int64 `json:"clock_ms"`
	} `json:"resolve_cases"`
	ReboundCase struct {
		Guard struct {
			Name     string
			Previous sendPin
			Next     fixtureCandidate
		}
		ClockOffsetMS     int64           `json:"clock_offset_ms"`
		Result            json.RawMessage `json:"result"`
		ResultWithoutNext json.RawMessage `json:"result_without_next"`
		NextLine          string          `json:"next_line"`
	} `json:"rebound_case"`
	PinCases []struct {
		Name, To string
		Guard    struct {
			Kind, Name string
			Pin        *sendPin
			Previous   *sendPin
			Next       *fixtureCandidate
		}
	} `json:"pin_cases"`
	BlockCases []struct {
		Name               string
		Data               json.RawMessage
		HandbackProvenance bool `json:"handback_provenance"`
		Block              json.RawMessage
	} `json:"block_cases"`
	LaneCases []struct {
		Name, Caller, To, Message string
		Result                    json.RawMessage
		Pending                   json.RawMessage
	} `json:"lane_cases"`
	ProtocolFrameCases []struct {
		Message  string
		Protocol bool
	} `json:"protocol_frame_cases"`
	ResumeRows map[string]struct {
		Message struct{ Content string }
		IsMeta  bool
		Origin  json.RawMessage
	} `json:"resume_rows"`
	InputCases []struct {
		Name, Wrapped string
		Origin        json.RawMessage
		Event         struct {
			PromptLength int `json:"prompt_length"`
		}
		Row struct {
			Content string
			IsMeta  bool
		}
	} `json:"input_cases"`
}

type fixtureCandidate struct {
	Name, ID, Kind string
	LastActive     *int64 `json:"lastActive"`
	Ref            string
}

// compactJSON removes the fixture's pretty-print whitespace; string escapes
// and member order are untouched, matching JSON.stringify output.
func compactJSON(t *testing.T, raw json.RawMessage) json.RawMessage {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func readSendMessageFixture(t *testing.T) sendMessageFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/send-message-native.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture sendMessageFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	for i := range fixture.WrapperCases {
		fixture.WrapperCases[i].Origin = compactJSON(t, fixture.WrapperCases[i].Origin)
	}
	for i := range fixture.PendingCases {
		fixture.PendingCases[i].Origin = compactJSON(t, fixture.PendingCases[i].Origin)
		fixture.PendingCases[i].Entry = compactJSON(t, fixture.PendingCases[i].Entry)
	}
	for i := range fixture.NotFoundCases {
		fixture.NotFoundCases[i].Result = compactJSON(t, fixture.NotFoundCases[i].Result)
	}
	for i := range fixture.ResolveCases {
		if len(fixture.ResolveCases[i].Result) != 0 && string(fixture.ResolveCases[i].Result) != "null" {
			fixture.ResolveCases[i].Result = compactJSON(t, fixture.ResolveCases[i].Result)
		}
	}
	fixture.ReboundCase.Result = compactJSON(t, fixture.ReboundCase.Result)
	fixture.ReboundCase.ResultWithoutNext = compactJSON(t, fixture.ReboundCase.ResultWithoutNext)
	for i := range fixture.BlockCases {
		fixture.BlockCases[i].Data = compactJSON(t, fixture.BlockCases[i].Data)
		fixture.BlockCases[i].Block = compactJSON(t, fixture.BlockCases[i].Block)
	}
	for i := range fixture.LaneCases {
		fixture.LaneCases[i].Result = compactJSON(t, fixture.LaneCases[i].Result)
		if len(fixture.LaneCases[i].Pending) != 0 && string(fixture.LaneCases[i].Pending) != "null" {
			fixture.LaneCases[i].Pending = compactJSON(t, fixture.LaneCases[i].Pending)
		}
	}
	if fixture.SDK != "00e5be0a8b69893cad9259a1e8b80d59be8f3eb367d4a16c19f91bcd279423b7" {
		t.Fatalf("unexpected pinned SDK %s", fixture.SDK)
	}
	return fixture
}

func fixtureBook(list []fixtureCandidate) ([]addressCandidate, *addressBook) {
	candidates := make([]addressCandidate, 0, len(list))
	for _, c := range list {
		candidate := addressCandidate{Name: c.Name, ID: c.ID, Kind: c.Kind}
		if c.LastActive != nil {
			candidate.LastActive, candidate.HasActive = time.UnixMilli(*c.LastActive), true
		}
		candidates = append(candidates, candidate)
	}
	assignRefs(candidates)
	return candidates, newAddressBook(candidates)
}

func TestSendMessageNativeConstants(t *testing.T) {
	f := readSendMessageFixture(t)
	c := f.Constants
	for name, pair := range map[string][2]string{
		"tag": {c.Tag, agentMessageTag}, "queued_main": {c.QueuedMain, textQueuedMain}, "main_self": {c.MainSelf, textMainSelf},
		"resumed_absent": {c.ResumedAbsent, textResumedReportAbsent}, "resumed_follows": {c.ResumedFollows, textResumedReportFollows},
		"resumed_withheld": {c.ResumedWithheld, textResumedWithheld}, "broadcast": {c.BroadcastUnsupport, textBroadcastUnsupported},
		"bare_name": {c.BareNameRequired, textBareNameRequired}, "message_empty": {c.MessageEmpty, textMessageEmpty},
		"protocol_frame": {c.ProtocolFrame, textProtocolFrame}, "lifecycle_frame": {c.LifecycleFrame, textLifecycleFrame},
		"peer_fresh": {c.PeerPrefixFresh, peerPrefixFresh}, "peer_mid_turn": {c.PeerPrefixMidTurn, peerPrefixMidTurn},
		"peer_trailer": {c.PeerTrailer, peerTrailer}, "peer_tail": {c.PeerMidTurnTail, peerMidTurnTail}, "coordinator": {c.CoordinatorPrefix, coordinatorPrefix},
		"agent_id": {c.AgentIDPattern, nativeAgentIDPattern.String()}, "address": {c.AddressPattern, nameRefPattern.String()},
	} {
		if pair[0] != pair[1] {
			t.Fatalf("%s differs from native: %q vs %q", name, pair[0], pair[1])
		}
	}
	if c.RefLength != refLength || c.RefMaxLength != refMaxLength || c.ClosestLimit != closestLimit || c.NameMaxCodePoints != originNameLimit ||
		c.ToMaxLength != toMaxLength || c.SummaryMaxLength != summaryMaxLength {
		t.Fatal("numeric SendMessage constants differ from native")
	}
	if strings.Join(c.ProtocolTypes, ",") != strings.Join(protocolFrameTypes, ",") || strings.Join(c.LifecycleTypes, ",") != strings.Join(lifecycleFrameTypes, ",") {
		t.Fatal("frame type lists differ from native")
	}
	for _, c := range f.ProtocolFrameCases {
		if got := containsString(protocolFrameTypes, frameType(c.Message)); got != c.Protocol {
			t.Fatalf("protocol frame %q: got %v", c.Message, got)
		}
	}
}

func TestSendMessageNativeEscapingClasses(t *testing.T) {
	f := readSendMessageFixture(t)
	if f.Regex.Open.Flags != "giu" || f.Regex.CloseOnly.Flags != "giu" {
		t.Fatalf("unexpected regex flags %q %q", f.Regex.Open.Flags, f.Regex.CloseOnly.Flags)
	}
	src := f.Regex.CloseOnly.Source
	open := regexp.MustCompile(`^\[([^\]]+)\]`).FindStringSubmatch(src)
	filler := regexp.MustCompile(`\[\^A-Za-z0-9_\\-([^\]]+)\]\*\)\)\(\?:\\1\)\[([^\]]+)\]`).FindStringSubmatch(src)
	invisible := regexp.MustCompile(`\(\?:a\(\?=\(\[([^\]]+)\]\*\)\)`).FindStringSubmatch(src)
	if open == nil || filler == nil || invisible == nil {
		t.Fatal("native regex source has an unexpected shape")
	}
	if open[1] != agentMessageOpenChars || filler[2] != agentMessageSlashChars || invisible[1] != agentMessageInvisibleClass {
		t.Fatal("tag escaping classes differ from the native regex source")
	}
	if !strings.HasPrefix(filler[1], agentMessageOpenChars+agentMessageCloseChars) || filler[1] != agentMessageOpenChars+agentMessageCloseChars+agentMessageSlashChars {
		t.Fatalf("filler class differs from native: %q", filler[1])
	}
	if !strings.HasSuffix(f.Regex.Open.Source, `e)(?:[^A-Za-z0-9_\-]|$))`) || !strings.Contains(f.Regex.Open.Source, `(?!\\)`) {
		t.Fatal("native boundary or backslash guard differs")
	}
	if len(agentMessageInvisible) == 0 || !inRanges(0x200b, agentMessageInvisible) || !inRanges(0xe0fff, agentMessageInvisible) || inRanges(0x0345, agentMessageInvisible) || !inRanges(0, agentMessageInvisible) {
		t.Fatal("invisible class parsed incorrectly")
	}
}

func TestSendMessageNativeWrapperAndProjections(t *testing.T) {
	f := readSendMessageFixture(t)
	for _, c := range f.WrapperCases {
		wrapped, origin := peerDelivery(c.From, "a0123456789abcdef", c.Message)
		if wrapped != c.Wrapped || origin.Body != c.Body || origin.Name != c.Name || originName(c.From) != c.Name {
			t.Fatalf("wrapper case %q: got %q name %q body %q", c.From, wrapped, origin.Name, origin.Body)
		}
		if got := escapeAgentMessageTags(c.Message, true); got != c.BodyCloseOnly {
			t.Fatalf("close-only escaping for %q: got %q want %q", c.Message, got, c.BodyCloseOnly)
		}
		if raw := jsonStringify(origin); !bytes.Equal(raw, c.Origin) {
			t.Fatalf("origin for %q: got %s want %s", c.From, raw, c.Origin)
		}
	}
	for _, c := range f.ProjectionCases {
		var origin messageOrigin
		if err := json.Unmarshal(c.Origin, &origin); err != nil {
			t.Fatal(err)
		}
		wrapped := c.Message
		if origin.Kind == "peer" {
			wrapped, _ = peerDelivery(c.From, origin.SenderTaskID, c.Message)
		}
		if wrapped != c.Wrapped {
			t.Fatalf("%s wrapped: got %q", c.Name, wrapped)
		}
		if got := freshTurnProjection(wrapped, origin); got != c.FreshString {
			t.Fatalf("%s fresh projection: got %q", c.Name, got)
		}
		if got := midTurnProjection(wrapped, origin); got != c.MidTurn {
			t.Fatalf("%s mid-turn projection: got %q", c.Name, got)
		}
		if got := queuedDeliveryWire(wrapped, origin); got != "<system-reminder>\n"+c.PendingWire+"\n</system-reminder>" {
			t.Fatalf("%s pending wire: got %q", c.Name, got)
		}
		if c.MidTurnIdempotent != nil && projectPeerMessage(c.MidTurn, true) != *c.MidTurnIdempotent {
			t.Fatalf("%s mid-turn projection is not idempotent", c.Name)
		}
	}
	for _, c := range f.PendingCases {
		var origin messageOrigin
		if err := json.Unmarshal(c.Origin, &origin); err != nil {
			t.Fatal(err)
		}
		if got := queuedDeliveryWire(c.Wrapped, origin); got != c.Wire[0].Message.Content || !c.Wire[0].IsMeta {
			t.Fatalf("%s wire: got %q", c.Name, got)
		}
		entry := jsonStringify(pendingMessage{Text: c.Wrapped, Origin: origin, IsMeta: true})
		if !bytes.Equal(entry, c.Entry) {
			t.Fatalf("%s pending entry: got %s want %s", c.Name, entry, c.Entry)
		}
		attachment := jsonStringify(struct {
			Type       string        `json:"type"`
			Prompt     string        `json:"prompt"`
			SourceUUID string        `json:"source_uuid"`
			Origin     messageOrigin `json:"origin"`
			IsMeta     bool          `json:"isMeta"`
		}{"queued_command", c.Wrapped, c.Wire[0].UUID, origin, true})
		if !bytes.Equal(attachment, compactJSON(t, c.Attachment)) {
			t.Fatalf("%s attachment: got %s want %s", c.Name, attachment, compactJSON(t, c.Attachment))
		}
	}
	for _, c := range f.InputCases {
		var origin messageOrigin
		if err := json.Unmarshal(c.Origin, &origin); err != nil {
			t.Fatal(err)
		}
		if got := FreshTurnWire(MainDelivery{Text: c.Wrapped, Origin: c.Origin}); got != c.Row.Content || !c.Row.IsMeta || c.Event.PromptLength != utf16Length(c.Wrapped) {
			t.Fatalf("%s main input projection: got %q", c.Name, got)
		}
	}
	for name, row := range f.ResumeRows {
		var origin messageOrigin
		if err := json.Unmarshal(row.Origin, &origin); err != nil {
			t.Fatal(err)
		}
		text := "continue"
		if origin.Kind == "peer" {
			text, _ = peerDelivery(origin.From, origin.SenderTaskID, origin.Body)
		}
		if got := midTurnProjection(text, origin); got != row.Message.Content || !row.IsMeta {
			t.Fatalf("resume row %s: got %q", name, got)
		}
	}
}

func TestSendMessageNativeAddressing(t *testing.T) {
	f := readSendMessageFixture(t)
	for _, c := range f.NormalizeCases {
		if got := normalizeAgentName(c.Input); got != c.Normalized {
			t.Fatalf("normalize %q: got %q want %q", c.Input, got, c.Normalized)
		}
	}
	for _, c := range f.ClosestCases {
		if got := strings.Join(closestNames(c.Query, c.Names), ","); got != strings.Join(c.Closest, ",") {
			t.Fatalf("closest %q: got %q want %q", c.Query, got, strings.Join(c.Closest, ","))
		}
	}
	for _, c := range f.RefCases {
		list := make([]addressCandidate, 0, len(c.Candidates))
		for _, candidate := range c.Candidates {
			list = append(list, addressCandidate{Kind: candidate.Kind, ID: candidate.ID})
		}
		assignRefs(list)
		for i := range list {
			if list[i].Ref != c.Refs[i] || candidateHash(list[i].Kind, list[i].ID) != c.Hashes[i] {
				t.Fatalf("ref %d: got %q/%q want %q/%q", i, list[i].Ref, candidateHash(list[i].Kind, list[i].ID), c.Refs[i], c.Hashes[i])
			}
		}
	}
	for _, c := range f.DurationCases {
		if got := formatDuration(time.Duration(c.MS) * time.Millisecond); got != c.Text {
			t.Fatalf("duration %d: got %q want %q", c.MS, got, c.Text)
		}
	}
	for _, c := range f.NotFoundCases {
		_, book := fixtureBook(c.Candidates)
		resolved := book.resolveAddress(c.To)
		if resolved.Kind != "not-found" {
			t.Fatalf("not-found %q resolved as %s", c.To, resolved.Kind)
		}
		raw, err := notFoundResult(c.To, resolved.Closest)
		if err != nil || !bytes.Equal(raw, c.Result) {
			t.Fatalf("not-found %q: got %s want %s", c.To, raw, c.Result)
		}
	}
	var (
		child  = "a0123456789abcdef"
		other  = "a89abcdef0123456"
		named  = "aworker-00112233445566778"
		mainID = "11111111-2222-4333-8444-555555555555"
	)
	registry := []fixtureCandidate{{Name: "main", ID: mainID, Kind: "main"}, {Name: "worker", ID: child, Kind: "subagent", LastActive: ptr(int64(1000))},
		{Name: "workshop", ID: other, Kind: "subagent", LastActive: ptr(int64(61000))}, {Name: "reviewer", ID: named, Kind: "subagent", LastActive: ptr(int64(2000))}}
	_, book := fixtureBook(registry)
	for _, c := range f.ResolveCases {
		if normalizeAgentName(c.To) != c.Normalized {
			t.Fatalf("resolve %q normalized %q", c.To, normalizeAgentName(c.To))
		}
		resolved := book.resolveAddress(c.To)
		switch c.Resolved.Kind {
		case "main":
			if resolved.Kind != "main" {
				t.Fatalf("resolve %q: got %s want main", c.To, resolved.Kind)
			}
		case "agent-live", "agent-stopped":
			if resolved.Kind != "agent" || resolved.ID != c.Resolved.AgentID || resolved.Name != c.Resolved.AgentName {
				t.Fatalf("resolve %q: got %+v", c.To, resolved)
			}
		case "ambiguous":
			if resolved.Kind != "ambiguous" || len(resolved.Candidates) != len(c.Resolved.Candidates) {
				t.Fatalf("resolve %q: got %+v", c.To, resolved)
			}
			raw, err := ambiguousResult(c.To, resolved.Candidates, time.UnixMilli(c.ClockMS))
			if err != nil || !bytes.Equal(raw, c.Result) {
				t.Fatalf("ambiguous %q: got %s want %s", c.To, raw, c.Result)
			}
		case "not-found":
			if resolved.Kind != "not-found" {
				t.Fatalf("resolve %q: got %s want not-found", c.To, resolved.Kind)
			}
			raw, err := notFoundResult(c.To, resolved.Closest)
			if err != nil || !bytes.Equal(raw, c.Result) {
				t.Fatalf("not-found %q: got %s want %s", c.To, raw, c.Result)
			}
		default:
			t.Fatalf("unexpected fixture kind %s", c.Resolved.Kind)
		}
	}
}

func ptr[T any](v T) *T { return &v }

func TestSendMessageNativePinGuard(t *testing.T) {
	f := readSendMessageFixture(t)
	child, rebound := "a0123456789abcdef", "a76543210fedcba98"
	pins := map[string]sendPin{}
	candidates := []fixtureCandidate{{Name: "main", ID: "11111111-2222-4333-8444-555555555555", Kind: "main"}, {Name: "Worker", ID: child, Kind: "subagent", LastActive: ptr(int64(1000))},
		{Name: "reviewer", ID: "aworker-00112233445566778", Kind: "subagent", LastActive: ptr(int64(2000))}}
	_, book := fixtureBook(candidates)
	resolvedFor := map[string]resolution{
		"main_no_pin": {Kind: "main"}, "first_send": {Kind: "agent", ID: child, Name: "Worker"}, "repeat_send": {Kind: "agent", ID: child, Name: "Worker"},
		"stopped_by_user_no_pin": {Kind: "agent", ID: "aworker-00112233445566778", Name: "reviewer"}, "id_send": {Kind: "agent", ID: child, Name: child},
		"rebound_same_spelling": {Kind: "agent", ID: rebound, Name: "Worker"}, "rebound_other_spelling": {Kind: "agent", ID: rebound, Name: "worker"},
	}
	for _, c := range f.PinCases {
		if c.Name == "rebound_same_spelling" {
			candidates[1] = fixtureCandidate{Name: "Worker", ID: rebound, Kind: "subagent", LastActive: ptr(int64(5000))}
			_, book = fixtureBook(candidates)
		}
		kind := ""
		if c.Name == "stopped_by_user_no_pin" {
			kind = "stopped-by-user"
		}
		decision := decidePin(pins, c.To, resolvedFor[c.Name], kind, book)
		if decision.SetPin {
			pins[decision.Key] = *decision.Pin
		}
		switch c.Guard.Kind {
		case "proceed":
			if decision.Rebound || (c.Guard.Pin == nil) != (decision.Pin == nil) || (c.Guard.Pin != nil && *c.Guard.Pin != *decision.Pin) {
				t.Fatalf("pin case %s: got %+v want pin %+v", c.Name, decision, c.Guard.Pin)
			}
		case "rebound":
			if !decision.Rebound || decision.Previous != *c.Guard.Previous || decision.Next == nil || decision.Next.ID != c.Guard.Next.ID || decision.Next.Ref != c.Guard.Next.Ref {
				t.Fatalf("pin case %s: got %+v", c.Name, decision)
			}
			now := time.UnixMilli(*c.Guard.Next.LastActive + f.ReboundCase.ClockOffsetMS)
			raw, err := reboundResult(c.Guard.Name, decision.Previous, decision.Next, now)
			if err != nil || !bytes.Equal(raw, f.ReboundCase.Result) {
				t.Fatalf("rebound result: got %s want %s", raw, f.ReboundCase.Result)
			}
			if describeCandidate(*decision.Next, now) != f.ReboundCase.NextLine {
				t.Fatalf("rebound line: got %q", describeCandidate(*decision.Next, now))
			}
			raw, err = reboundResult(c.Guard.Name, decision.Previous, nil, now)
			if err != nil || !bytes.Equal(raw, f.ReboundCase.ResultWithoutNext) {
				t.Fatalf("rebound without next: got %s", raw)
			}
		default:
			t.Fatalf("unexpected guard kind %s", c.Guard.Kind)
		}
	}
}

func TestSendMessageNativeBlockMapping(t *testing.T) {
	f := readSendMessageFixture(t)
	frame := func(json.RawMessage) (string, error) { return "SYNTHETIC_FRAMED_HANDBACK", nil }
	for _, c := range f.BlockCases {
		reads := 0
		policy := func() bool { reads++; return c.HandbackProvenance }
		block, err := sendMessageBlock("toolu_01", c.Data, policy, frame)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if !bytes.Equal(block, c.Block) {
			t.Fatalf("%s: got %s want %s", c.Name, block, c.Block)
		}
		// Native reads the hand-back feature only when an inline hand-back exists.
		if handback := bytes.Contains(c.Data, []byte(`"inlineHandback"`)); (reads == 1) != handback {
			t.Fatalf("%s: feature reads=%d with inlineHandback=%v", c.Name, reads, handback)
		}
	}
	never := func() bool { t.Fatal("feature read without a hand-back"); return false }
	if _, err := sendMessageBlock("toolu_01", json.RawMessage(`[1]`), never, frame); err == nil || err.Error() != "SendMessage output is not an object" {
		t.Fatalf("array output: %v", err)
	}
	if _, err := sendMessageBlock("toolu_01", json.RawMessage(`null`), never, frame); err == nil {
		t.Fatal("null output accepted")
	}
	// The framed and withheld lanes drop every other member, including a pin.
	withPin := json.RawMessage(`{"success":true,"message":"Resumed agent. Its final report is not in this message.","inlineHandback":{"displayName":"w","content":[{"type":"text","text":"x"}],"harnessNoteCount":0,"harnessTailCount":0,"harnessSectionHash":"h"},"pin":{"id":"a0123456789abcdef","name":"w","ref":"7cedb0"}}`)
	block, err := sendMessageBlock("toolu_01", withPin, func() bool { return true }, frame)
	if err != nil || !bytes.Contains(block, []byte(`{\"success\":true,\"message\":\"`+textResumedReportFollows+`\"}\nSYNTHETIC_FRAMED_HANDBACK`)) || bytes.Contains(block, []byte("pin")) {
		t.Fatalf("framed hand-back kept extra members: %s", block)
	}
	block, err = sendMessageBlock("toolu_01", json.RawMessage(`{"success":true,"message":"Resuming agent w","handoffReviewSkipped":true,"pin":{"id":"a","name":"w","ref":"r"}}`), never, frame)
	if err != nil || !bytes.Contains(block, []byte(`{\"success\":true,\"message\":\"`+textResumedWithheld+`\"}`)) || bytes.Contains(block, []byte("pin")) {
		t.Fatalf("withheld lane kept extra members: %s", block)
	}
	var r *Runtime
	block = r.ToolResult(ToolCall{ID: "toolu_01", Name: "SendMessage"}, json.RawMessage(`{"success":true,"message":"Message queued for the main conversation's next turn."}`), nil)
	if string(block) != `{"tool_use_id":"toolu_01","type":"tool_result","content":[{"type":"text","text":"{\"success\":true,\"message\":\"Message queued for the main conversation's next turn.\"}"}]}` {
		t.Fatalf("runtime block: %s", block)
	}
}

func TestSendMessageNativeLaneTexts(t *testing.T) {
	f := readSendMessageFixture(t)
	child, other, named := "a0123456789abcdef", "a89abcdef0123456", "aworker-00112233445566778"
	pin := func(id, name string) *sendPin {
		return &sendPin{ID: id, Name: name, Ref: candidateHash("subagent", id)[:refLength]}
	}
	got := map[string]json.RawMessage{}
	raw := func(result sendResult) json.RawMessage { data, _ := result.raw(); return data }
	got["main_from_main"] = raw(sendResult{Success: false, Message: textMainSelf})
	got["main_from_child"] = raw(sendResult{Success: true, Message: textQueuedMain})
	got["live_from_main"] = raw(sendResult{Success: true, Message: "Message queued for delivery to worker at its next tool round.", Pin: pin(child, "worker")})
	got["live_self"] = raw(sendResult{Success: true, Message: "Message queued for delivery to " + child + " at its next tool round.", Pin: pin(child, child)})
	got["stopped_by_user"] = raw(sendResult{Success: false, Message: "Agent \"reviewer\" was stopped by the user and was not resumed. Treat its work as cancelled; only start a new agent for it if the user explicitly asks."})
	got["resume_from_main"] = raw(sendResult{Success: true, Message: "Resuming agent " + resumeDisplayName("reviewer"), ResumedAgentID: named, Pin: pin(named, "reviewer")})
	got["resume_by_id"] = raw(sendResult{Success: true, Message: "Resuming agent " + resumeDisplayName(named), ResumedAgentID: named, Pin: pin(named, named)})
	got["resume_user_facing_failure"] = raw(sendResult{Success: false, Message: "Concurrent subagent limit reached"})
	for _, c := range f.LaneCases {
		expected, ok := got[c.Name]
		if !ok {
			continue
		}
		if !bytes.Equal(expected, c.Result) {
			t.Fatalf("lane %s: got %s want %s", c.Name, expected, c.Result)
		}
		if c.Name == "live_from_main" || c.Name == "live_self" {
			sender := ""
			if c.Caller != "" {
				sender = "worker"
			}
			text, origin := c.Message, coordinatorOrigin
			if sender != "" {
				text, origin = peerDelivery(sender, c.Caller, c.Message)
			}
			entry := jsonStringify([]pendingMessage{{Text: text, Origin: origin, IsMeta: true}})
			if !bytes.Equal(entry, c.Pending) {
				t.Fatalf("lane %s pending: got %s want %s", c.Name, entry, c.Pending)
			}
		}
	}
	_ = other
	if resumeDisplayName(child) != "a012345" || resumeDisplayName("reviewer") != "reviewer" || resumeDisplayName(named) != named {
		t.Fatal("resume display name differs from native nFt")
	}
	if !nativeAgentIDPattern.MatchString(newAgentID()) || nativeAgentIDPattern.MatchString(named) || !isAgentID("a1b2c3d4e") {
		t.Fatal("agent id generation or acceptance differs")
	}
}
