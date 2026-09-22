package prompt

// These are events owned by the proxy's canonical SDK message model, not a
// reconstruction of a caller's private SDK history. Desktop normalization can
// fold extra user text into a tool result, making that inverse ambiguous.
type sdkUserYieldKind uint8

const (
	sdkToolResultUserYield sdkUserYieldKind = iota
	sdkInterruptionUserYield
	sdkCompactionSummaryUserYield
)

func (s *sdkAccounting) yieldUser(kind sdkUserYieldKind) {
	s.numTurns++
	message := SDKHistoryMessage{Type: "user"}
	if kind == sdkInterruptionUserYield {
		// This is the reviewed stream-abort marker, not caller message content.
		message.TokenEstimate = SDKTokenEstimate{Tokens: sdkRoundedTokens(sdkTextUnits("[Request interrupted by user]")), Known: true}
	}
	s.history.append(message)
	switch kind {
	case sdkToolResultUserYield:
		s.toolResultUserYields++
	case sdkInterruptionUserYield:
		s.interruptionUserYields++
	case sdkCompactionSummaryUserYield:
		s.compactionSummaryUserYields++
	}
}

// materializeToolResultUsers runs once when a new logical continuation is
// accepted, before its HTTP query. The installed SDK's ordinary tool executor
// wraps each result in one user object, even if the wire normalizer later merges
// several such objects into one row. Retries return the same call before here.
// Only pure, uniquely owned tool-result tails reach this producer. It does not
// invent hook feedback, attachments, compaction summaries, or tool executions.
func (c *call) materializeToolResultUsers() {
	for _, id := range c.resultIDs {
		c.sdkUserHistoryOffsets = append(c.sdkUserHistoryOffsets, len(c.state.sdk.history.messages))
		c.state.sdk.yieldUser(sdkToolResultUserYield)
		if offset := c.sdkUserHistoryOffsets[len(c.sdkUserHistoryOffsets)-1]; offset < len(c.state.sdk.history.messages) {
			c.state.sdk.history.messages[offset].WireToolResultID = id
		}
	}
}

// Historical tool calls do not make every later cancellation a tool-drain
// cancellation. Already received results were materialized before this query;
// only unreturned IDs or tool blocks closed in this attempt remain uncertain.
func (r *Request) hasUnobservedSDKToolDrain() bool {
	if r.sdkToolsObserved != 0 {
		return true
	}
	returned := make(map[string]struct{}, len(r.call.resultIDs))
	for _, id := range r.call.resultIDs {
		returned[id] = struct{}{}
	}
	for id := range r.call.state.pending {
		if _, ok := returned[id]; !ok {
			return true
		}
	}
	return false
}
