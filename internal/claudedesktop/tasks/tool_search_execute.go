package tasks

import (
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// ToolSearch execution (jWe.call with Y3n/K3n/AQ) against the owned pool.
// The MCP refresh and wait paths never apply: the pool is static and has no
// MCP servers, so missing select names and empty keyword searches end here.

var (
	// selectQueryPattern is /^select:(.+)$/i; the class spells out the line
	// terminators a JavaScript dot never matches.
	selectQueryPattern = regexp.MustCompile(`(?i)^select:([^\n\r\x{2028}\x{2029}]+)$`)
	// camelBoundary is /([a-z])([A-Z])/g from K3n.
	camelBoundary = regexp.MustCompile(`([a-z])([A-Z])`)
)

// toolSearch parses the ToolSearch input and runs the select or keyword form.
func toolSearch(ctx DefinitionContext, input json.RawMessage) (json.RawMessage, error) {
	var call struct {
		Query      *string         `json:"query"`
		MaxResults json.RawMessage `json:"max_results"`
	}
	if json.Unmarshal(input, &call) != nil || call.Query == nil {
		return nil, errors.New("ToolSearch requires a query string and an optional numeric max_results")
	}
	maxResults := 5.0
	if len(call.MaxResults) > 0 {
		if json.Unmarshal(call.MaxResults, &maxResults) != nil || strings.TrimSpace(string(call.MaxResults)) == "null" {
			return nil, errors.New("ToolSearch requires a query string and an optional numeric max_results")
		}
	}
	query := *call.Query
	deferred := deferrableTools(ctx)
	if m := selectQueryPattern.FindStringSubmatch(query); m != nil {
		var matches []string
		for _, part := range strings.Split(m[1], ",") {
			name := strings.TrimFunc(part, jsTrimSpace)
			if name == "" {
				continue
			}
			tool, ok := lookupPoolTool(deferred, name)
			if !ok {
				tool, ok = lookupPoolTool(toolPool, name)
			}
			if ok && !slices.Contains(matches, tool.name) {
				matches = append(matches, tool.name)
			}
		}
		return toolSearchData(matches, query, len(deferred)), nil
	}
	return toolSearchData(keywordSearch(ctx, query, deferred, maxResults), query, len(deferred)), nil
}

// lookupPoolTool is ws(list, name) without a legacy map: the first tool whose
// name or aliases contain the exact name.
func lookupPoolTool(tools []poolTool, name string) (poolTool, bool) {
	for _, tool := range tools {
		if tool.matches(name) {
			return tool, true
		}
	}
	return poolTool{}, false
}

// toolSearchData is AQ(matches, query, total): the result data object.
func toolSearchData(matches []string, query string, totalDeferred int) json.RawMessage {
	var b strings.Builder
	b.WriteString(`{"matches":[`)
	for i, name := range matches {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(jsQuote(name))
	}
	b.WriteString(`],"query":`)
	b.WriteString(jsQuote(query))
	b.WriteString(`,"total_deferred_tools":`)
	b.WriteString(strconv.Itoa(totalDeferred))
	b.WriteByte('}')
	return json.RawMessage(b.String())
}

// searchNameParts is K3n for a non-MCP tool: camel-case and underscore
// splits of the name, the whole lowercased name, and the joined parts.
func searchNameParts(name string) (parts, coarse []string, full string) {
	spaced := strings.ReplaceAll(camelBoundary.ReplaceAllString(name, "${1} ${2}"), "_", " ")
	parts = strings.FieldsFunc(strings.ToLower(spaced), jsTrimSpace)
	return parts, []string{strings.ToLower(name)}, strings.Join(parts, " ")
}

func anyContains(values []string, term string) bool {
	for _, value := range values {
		if strings.Contains(value, term) {
			return true
		}
	}
	return false
}

// keywordSearch is Y3n: an exact lowercase name match wins outright;
// otherwise the deferred tools are scored per term against the name parts,
// the search hint and the prompt text, required (+) terms filter first, and
// the stable score order is cut to max_results.
func keywordSearch(ctx DefinitionContext, query string, deferred []poolTool, maxResults float64) []string {
	needle := strings.TrimFunc(strings.ToLower(query), jsTrimSpace)
	for _, tools := range [][]poolTool{deferred, toolPool} {
		for _, tool := range tools {
			if strings.ToLower(tool.name) == needle {
				return []string{tool.name}
			}
		}
	}
	tokens := strings.FieldsFunc(needle, jsTrimSpace)
	var required, others []string
	for _, token := range tokens {
		if strings.HasPrefix(token, "+") && len(token) > 1 {
			required = append(required, token[1:])
		} else {
			others = append(others, token)
		}
	}
	terms := tokens
	if len(required) > 0 {
		terms = append(append(make([]string, 0, len(tokens)), required...), others...)
	}
	patterns := make(map[string]*regexp.Regexp, len(terms))
	for _, term := range terms {
		if _, ok := patterns[term]; !ok {
			patterns[term] = regexp.MustCompile(`\b` + regexp.QuoteMeta(term) + `\b`)
		}
	}
	type scored struct {
		name  string
		score int
	}
	var results []scored
	for _, tool := range deferred {
		parts, coarse, full := searchNameParts(tool.name)
		description := strings.ToLower(tool.prompt(ctx))
		hint := strings.ToLower(tool.searchHint)
		hits := func(term string) bool {
			pattern := patterns[term]
			return slices.Contains(parts, term) || anyContains(parts, term) || slices.Contains(coarse, term) || anyContains(coarse, term) ||
				pattern.MatchString(description) || (hint != "" && pattern.MatchString(hint))
		}
		if len(required) > 0 && slices.ContainsFunc(required, func(term string) bool { return !hits(term) }) {
			continue
		}
		score := 0
		for _, term := range terms {
			pattern := patterns[term]
			if slices.Contains(parts, term) {
				score += 10
			} else if anyContains(parts, term) {
				score += 5
			}
			if slices.Contains(coarse, term) {
				score += 10
			} else if anyContains(coarse, term) {
				score += 3
			}
			if strings.Contains(full, term) && score == 0 {
				score += 3
			}
			if hint != "" && pattern.MatchString(hint) {
				score += 4
			}
			if pattern.MatchString(description) {
				score += 2
			}
		}
		if score > 0 {
			results = append(results, scored{name: tool.name, score: score})
		}
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].score > results[j].score })
	names := make([]string, 0, len(results))
	for _, result := range results[:jsSliceEnd(maxResults, len(results))] {
		names = append(names, result.name)
	}
	return names
}

// jsSliceEnd resolves Array.prototype.slice(0, end) for a JavaScript number.
func jsSliceEnd(end float64, length int) int {
	if math.IsNaN(end) {
		return 0
	}
	end = math.Trunc(end)
	if end < 0 {
		return int(math.Max(float64(length)+end, 0))
	}
	return int(math.Min(end, float64(length)))
}

// toolSearchResultContent maps the data to the tool_result content:
// tool_reference blocks for matches, the fixed string when there are none.
func toolSearchResultContent(data json.RawMessage) (any, error) {
	var value struct {
		Matches []string `json:"matches"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	if len(value.Matches) == 0 {
		return noMatchingDeferredTools, nil
	}
	blocks := make([]toolReferenceBlock, 0, len(value.Matches))
	for _, name := range value.Matches {
		blocks = append(blocks, toolReferenceBlock{Type: "tool_reference", ToolName: name})
	}
	return blocks, nil
}

// toolReferenceBlock keeps the native member order type, tool_name.
type toolReferenceBlock struct {
	Type     string `json:"type"`
	ToolName string `json:"tool_name"`
}
