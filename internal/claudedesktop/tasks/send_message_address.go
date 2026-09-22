package tasks

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"

	"golang.org/x/text/unicode/norm"
)

// Native SendMessage addressing (Claude Code 2.1.247): agent identifiers,
// candidate refs, name normalization, "name [ref]" addresses, resolution
// lanes, closest-name suggestions and the per-conversation pin guard.

const (
	mainAgentName  = "main"
	refLength      = 6
	refMaxLength   = 12
	closestLimit   = 3
	prefixMinChars = 3
)

// nativeAgentIDPattern is the identifier the native runtime generates and
// accepts as a direct SendMessage target.
var nativeAgentIDPattern = regexp.MustCompile(`^a(?:[\w-]{1,63}-)?[0-9a-f]{16}$`)

// legacyAgentIDPattern matches identifiers persisted by earlier local builds.
var legacyAgentIDPattern = regexp.MustCompile(`^a[0-9a-z]{8}$`)

var nameRefPattern = regexp.MustCompile(`^(.*\S)\s*\[([0-9a-f]{6,12})\]$`)

func isAgentID(value string) bool {
	return nativeAgentIDPattern.MatchString(value) || legacyAgentIDPattern.MatchString(value)
}

// newAgentID follows the native generator: "a" plus 16 lowercase hex digits.
func newAgentID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return ""
	}
	return "a" + hex.EncodeToString(buf[:])
}

type addressCandidate struct {
	Name       string
	ID         string
	Kind       string
	LastActive time.Time
	HasActive  bool
	Ref        string
}

func candidateHash(kind, id string) string {
	sum := sha256.Sum256([]byte(kind + ":" + id))
	return hex.EncodeToString(sum[:])[:refMaxLength]
}

// assignRefs shortens each candidate hash to six hex digits or one digit past
// the longest shared prefix with its sorted neighbours.
func assignRefs(candidates []addressCandidate) {
	hashes := make([]string, len(candidates))
	unique := map[string]bool{}
	var sorted []string
	for i, c := range candidates {
		hashes[i] = candidateHash(c.Kind, c.ID)
		if !unique[hashes[i]] {
			unique[hashes[i]] = true
			sorted = append(sorted, hashes[i])
		}
	}
	sort.Strings(sorted)
	length := map[string]int{}
	for i, h := range sorted {
		shared := 0
		if i > 0 {
			shared = commonPrefix(h, sorted[i-1])
		}
		if i+1 < len(sorted) {
			if n := commonPrefix(h, sorted[i+1]); n > shared {
				shared = n
			}
		}
		n := shared + 1
		if n < refLength {
			n = refLength
		}
		if n > len(h) {
			n = len(h)
		}
		length[h] = n
	}
	for i := range candidates {
		candidates[i].Ref = hashes[i][:length[hashes[i]]]
	}
}

func commonPrefix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

var nameSpaces = regexp.MustCompile(`[\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]+`)

// normalizeAgentName is the native case/whitespace-insensitive key: NFKC,
// drop control and format code points that are not whitespace, trim, lower
// case and join whitespace runs with "-".
func normalizeAgentName(name string) string {
	var b strings.Builder
	for _, r := range norm.NFKC.String(name) {
		if unicode.In(r, unicode.Cc, unicode.Cf) && !jsTrimSpace(r) {
			continue
		}
		b.WriteRune(r)
	}
	lowered := strings.ToLower(strings.TrimFunc(b.String(), jsTrimSpace))
	return nameSpaces.ReplaceAllString(lowered, "-")
}

// parseNameRef splits a "name [ref]" address; the ref keeps its spelling so a
// mismatched case never resolves.
func parseNameRef(to string) (name, ref string, ok bool) {
	m := nameRefPattern.FindStringSubmatch(strings.TrimFunc(to, jsTrimSpace))
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

func formatDuration(d time.Duration) string {
	ms := d.Milliseconds()
	if ms < 0 {
		ms = 0
	}
	if ms < 60000 {
		return itoa(ms/1000) + "s"
	}
	days, hours, minutes := ms/86400000, ms%86400000/3600000, ms%3600000/60000
	seconds := (ms%60000 + 500) / 1000
	if seconds == 60 {
		seconds, minutes = 0, minutes+1
	}
	if minutes == 60 {
		minutes, hours = 0, hours+1
	}
	if hours == 24 {
		hours, days = 0, days+1
	}
	switch {
	case days > 0:
		return itoa(days) + "d"
	case hours > 0:
		return itoa(hours) + "h"
	case minutes > 0:
		return itoa(minutes) + "m"
	}
	return itoa(seconds) + "s"
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func candidateAddress(c addressCandidate) string { return c.Name + " [" + c.Ref + "]" }

// describeCandidate is the native list line: address, kind, location and the
// coarse start age for subagents.
func describeCandidate(c addressCandidate, now time.Time) string {
	kind := c.Kind
	if kind == mainAgentName {
		kind = "main conversation"
	}
	suffix := ""
	if c.HasActive {
		suffix = ", started " + formatDuration(now.Sub(c.LastActive)) + " ago"
	}
	return candidateAddress(c) + " \u2014 " + kind + ", in this session" + suffix
}

// editDistance is the native Damerau-Levenshtein over UTF-16 code units.
func editDistance(a, b string) int {
	if a == b {
		return 0
	}
	x, y := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	rows := make([][]int, len(x)+1)
	for i := range rows {
		rows[i] = make([]int, len(y)+1)
		rows[i][0] = i
	}
	for j := range rows[0] {
		rows[0][j] = j
	}
	for i := 1; i <= len(x); i++ {
		for j := 1; j <= len(y); j++ {
			cost := 1
			if x[i-1] == y[j-1] {
				cost = 0
			}
			best := rows[i-1][j] + 1
			if v := rows[i][j-1] + 1; v < best {
				best = v
			}
			if v := rows[i-1][j-1] + cost; v < best {
				best = v
			}
			if i > 1 && j > 1 && x[i-1] == y[j-2] && x[i-2] == y[j-1] {
				if v := rows[i-2][j-2] + 1; v < best {
					best = v
				}
			}
			rows[i][j] = best
		}
	}
	return rows[len(x)][len(y)]
}

// closestNames returns up to closestLimit distinct names within two edits.
func closestNames(query string, names []string) []string {
	type scored struct {
		name     string
		distance int
	}
	seen := map[string]bool{}
	var found []scored
	queryLength := len(utf16.Encode([]rune(query)))
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		diff := len(utf16.Encode([]rune(name))) - queryLength
		if diff > 2 || diff < -2 {
			continue
		}
		if d := editDistance(query, name); d <= 2 {
			found = append(found, scored{name, d})
		}
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].distance < found[j].distance })
	if len(found) > closestLimit {
		found = found[:closestLimit]
	}
	out := make([]string, 0, len(found))
	for _, f := range found {
		out = append(out, f.name)
	}
	return out
}

// resolution is the outcome of the native recipient lanes for in-process
// agents. Kind is one of main, agent, ambiguous, not-found.
type resolution struct {
	Kind       string
	ID         string
	Name       string
	Candidates []addressCandidate
	MatchedBy  string
	Closest    []addressCandidate
}

type addressBook struct {
	candidates []addressCandidate
	byName     map[string][]addressCandidate
	order      []string
}

func newAddressBook(candidates []addressCandidate) *addressBook {
	book := &addressBook{candidates: candidates, byName: map[string][]addressCandidate{}}
	for _, c := range candidates {
		key := normalizeAgentName(c.Name)
		if _, ok := book.byName[key]; !ok {
			book.order = append(book.order, key)
		}
		book.byName[key] = append(book.byName[key], c)
	}
	return book
}

func (b *addressBook) one(key, name string) (addressCandidate, bool) {
	list := b.byName[key]
	if len(list) == 0 {
		return addressCandidate{}, false
	}
	if name != "" {
		for _, c := range list {
			if c.Name == name {
				return c, true
			}
		}
	}
	return list[0], true
}

func (b *addressBook) closest(query string) []addressCandidate {
	names := make([]string, 0, len(b.candidates))
	for _, c := range b.candidates {
		names = append(names, c.Name)
	}
	var out []addressCandidate
	for _, name := range closestNames(query, names) {
		for _, c := range b.candidates {
			if c.Name == name {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

func resolveCandidate(c addressCandidate) resolution {
	if c.Kind == mainAgentName {
		return resolution{Kind: mainAgentName}
	}
	return resolution{Kind: "agent", ID: c.ID, Name: c.Name}
}

// resolveAddress runs the native lanes after the registry's exact name and
// agent-ID checks: "name [ref]", case-folded name, unique prefix, ambiguous
// prefix and not-found with closest suggestions.
func (b *addressBook) resolveAddress(to string) resolution {
	if name, ref, ok := parseNameRef(to); ok {
		if list := b.byName[normalizeAgentName(name)]; len(list) > 0 {
			for _, c := range list {
				if c.Ref == ref {
					return resolveCandidate(c)
				}
			}
		}
		return resolution{Kind: "not-found", Closest: b.closest(name)}
	}
	normalized := normalizeAgentName(to)
	if c, ok := b.one(normalized, to); ok {
		return resolveCandidate(c)
	}
	if utf16Length(normalized) >= prefixMinChars {
		var keys []string
		for _, key := range b.order {
			if strings.HasPrefix(key, normalized) {
				keys = append(keys, key)
			}
		}
		if len(keys) == 1 {
			if c, ok := b.one(keys[0], ""); ok {
				return resolveCandidate(c)
			}
		}
		if len(keys) > 1 {
			var matches []addressCandidate
			for _, key := range keys {
				matches = append(matches, b.byName[key]...)
			}
			return resolution{Kind: "ambiguous", Candidates: matches, MatchedBy: "prefix"}
		}
	}
	return resolution{Kind: "not-found", Closest: b.closest(to)}
}

func utf16Length(s string) int { return len(utf16.Encode([]rune(s))) }

// sendPin is the per-conversation binding recorded after a successful send.
type sendPin struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Ref  string `json:"ref"`
}
