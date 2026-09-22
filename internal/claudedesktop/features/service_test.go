package features

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync"
	"testing"
)

type memoryStore struct {
	payloads         map[string][]byte
	revisions        map[string]string
	loadErr, saveErr error
	writes           int
	trace            *[]string
}

func (s *memoryStore) Load(scope string) ([]byte, string, error) {
	return bytes.Clone(s.payloads[scope]), s.revisions[scope], s.loadErr
}

func (s *memoryStore) Save(scope, previous string, payload []byte) (string, error) {
	if s.saveErr != nil {
		return "", s.saveErr
	}
	if s.revisions[scope] != previous {
		return "", errors.New("stale cache revision")
	}
	if s.payloads == nil {
		s.payloads = make(map[string][]byte)
		s.revisions = make(map[string]string)
	}
	s.writes++
	s.payloads[scope], s.revisions[scope] = bytes.Clone(payload), fmt.Sprint(s.writes)
	if s.trace != nil {
		*s.trace = append(*s.trace, "disk")
	}
	return s.revisions[scope], nil
}

func feature(value, experiment, variation string) string {
	return fmt.Sprintf(`{"value":%s,"source":"experiment","experiment":{"key":%q},"experimentResult":{"variationId":%s}}`, value, experiment, variation)
}

func payload(entries string) []byte { return []byte(`{"features":{` + entries + `}}`) }

func bind(t *testing.T, s *Service, scope, token string) Ticket {
	t.Helper()
	ticket, err := s.Bind(scope, token)
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}

func observe(t *testing.T, s *Service, ticket Ticket, p []byte, sink Sink) {
	t.Helper()
	accepted, err := s.Observe(ticket, p, "session", sink)
	if err != nil || !accepted {
		t.Fatalf("observe = %v, %v", accepted, err)
	}
}

func lookup(t *testing.T, s *Service, ticket Ticket, name, wantRaw, wantSource string, sink Sink) {
	t.Helper()
	got, err := s.Lookup(ticket, "session", name, json.RawMessage(`true`), sink)
	if err != nil || !equalJSON(got.Raw, json.RawMessage(wantRaw)) || got.Source != wantSource {
		t.Fatalf("lookup %s = %s/%s, %v; want %s/%s", name, got.Raw, got.Source, err, wantRaw, wantSource)
	}
}

func capture(records *[]Exposure) Sink {
	return func(e Exposure) (bool, error) { *records = append(*records, e); return true, nil }
}

func seeded(t *testing.T, entries string) (*memoryStore, *Service, Ticket) {
	t.Helper()
	store := &memoryStore{}
	previous := New(store)
	observe(t, previous, bind(t, previous, "account", "token"), payload(entries), nil)
	s := New(store)
	return store, s, bind(t, s, "account", "token")
}

func TestFeatureReadOwnsExposureAndRefreshPreservesDedup(t *testing.T) {
	s := New(nil)
	ticket := bind(t, s, "account", "token")
	var records []Exposure
	sink := capture(&records)
	observe(t, s, ticket, payload(`"flag":`+feature(`false`, "experiment", `0`)+`,"other":`+feature(`null`, "experiment", `1`)), sink)
	if len(records) != 0 {
		t.Fatal("evaluation emitted an exposure")
	}
	lookup(t, s, ticket, "flag", "false", "payload", sink)
	lookup(t, s, ticket, "flag", "false", "payload", sink)
	lookup(t, s, ticket, "other", "true", "payload", sink)
	observe(t, s, ticket, payload(`"flag":`+feature(`true`, "new-experiment", `2`)), sink)
	lookup(t, s, ticket, "flag", "true", "payload", sink)
	if len(records) != 2 || records[0] != (Exposure{"session", "flag", "experiment", 0}) || records[1].FeatureID != "other" {
		t.Fatalf("exposures = %+v", records)
	}
}

func TestFeatureMetadataValidationAndValueNormalization(t *testing.T) {
	for _, entry := range []string{
		`{"value":false}`, `{"value":false,"source":"defaultValue"}`,
		`{"value":false,"source":"experiment","experiment":{"key":1},"experimentResult":{"variationId":0}}`,
		`{"value":false,"source":"experiment","experiment":{"key":"x"},"experimentResult":{"variationId":"0"}}`,
		`{"value":false,"source":"experiment","experiment":{"key":null},"experimentResult":{"variationId":0}}`,
	} {
		t.Run(entry, func(t *testing.T) {
			s := New(nil)
			ticket := bind(t, s, "a", "t")
			var records []Exposure
			observe(t, s, ticket, payload(`"flag":`+entry), capture(&records))
			lookup(t, s, ticket, "flag", "false", "payload", capture(&records))
			if len(records) != 0 {
				t.Fatal(records)
			}
		})
	}
	for _, entry := range []string{
		feature(`false`, "", `0`),
		`{"defaultValue":false,"source":"experiment","experiment":{"key":"x"},"experimentResult":{"variationId":1}}`,
		`{"value":false,"source":"experi\u006dent","experiment":{"key":"x"},"experimentResult":{"variationId":-1.5}}`,
	} {
		t.Run(entry, func(t *testing.T) {
			s := New(nil)
			ticket := bind(t, s, "a", "t")
			var records []Exposure
			observe(t, s, ticket, payload(`"flag":`+entry), nil)
			lookup(t, s, ticket, "flag", "false", "payload", capture(&records))
			if len(records) != 1 {
				t.Fatal(records)
			}
		})
	}
}

func TestFeatureValuelessRefreshRetainsFacts(t *testing.T) {
	s := New(nil)
	ticket := bind(t, s, "a", "t")
	observe(t, s, ticket, payload(`"flag":{"value":false}`), nil)
	for _, p := range []string{`null`, `[]`, `true`, `{}`, `{"features":null}`, `{"features":{"flag":{}}}`, `{"features":[]}`} {
		accepted, err := s.Observe(ticket, []byte(p), "session", nil)
		if accepted || err != nil {
			t.Fatalf("%s: %v %v", p, accepted, err)
		}
		lookup(t, s, ticket, "flag", "false", "payload", nil)
	}
	if _, err := s.Observe(ticket, []byte(`{"features":`), "session", nil); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	observe(t, s, ticket, []byte(`{"features":[{"value":0}]}`), nil)
	lookup(t, s, ticket, "0", "0", "payload", nil)
}

func TestFeatureSinkAcknowledgmentAndPolicyRecursion(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(fmt.Sprint(disabled), func(t *testing.T) {
			s := New(nil)
			ticket := bind(t, s, "a", "t")
			var records []Exposure
			policy := fmt.Sprintf(`{"firstParty":%v}`, disabled)
			observe(t, s, ticket, payload(`"flag":`+feature(`false`, "main-exp", `0`)+`,"tengu_frond_boric":`+feature(policy, "policy-exp", `1`)), nil)
			lookup(t, s, ticket, "flag", "false", "payload", capture(&records))
			if disabled {
				if len(records) != 0 || len(s.logged) != 2 {
					t.Fatalf("records=%v logged=%v", records, s.logged)
				}
			} else if len(records) != 2 || records[0].FeatureID != "tengu_frond_boric" || records[1].FeatureID != "flag" {
				t.Fatalf("recursive ordering: %v", records)
			}
		})
	}
	for _, mode := range []string{"nil", "not-ready", "failure", "disabled-ack"} {
		t.Run(mode, func(t *testing.T) {
			s := New(nil)
			ticket := bind(t, s, "a", "t")
			var records []Exposure
			observe(t, s, ticket, payload(`"flag":`+feature(`false`, "exp", `0`)), nil)
			var sink Sink
			if mode != "nil" {
				sink = func(Exposure) (bool, error) {
					if mode == "failure" {
						return false, errors.New("queue failed")
					}
					return mode == "disabled-ack", nil
				}
			}
			_, err := s.Lookup(ticket, "session", "flag", nil, sink)
			if (err != nil) != (mode == "failure") {
				t.Fatalf("error=%v", err)
			}
			lookup(t, s, ticket, "flag", "false", "payload", capture(&records))
			want := 1
			if mode == "disabled-ack" {
				want = 0
			}
			if len(records) != want {
				t.Fatal(records)
			}
		})
	}
}

func TestFeatureDisabledServiceIsNotDisabledLogger(t *testing.T) {
	s := New(nil)
	ticket := bind(t, s, "a", "t")
	var records []Exposure
	observe(t, s, ticket, payload(`"flag":`+feature(`false`, "exp", `0`)), nil)
	s.SetEnabled(false, false)
	lookup(t, s, ticket, "flag", "true", "disabled", capture(&records))
	s.SetEnabled(false, true)
	lookup(t, s, ticket, "flag", "false", "payload", capture(&records))
	if len(records) != 1 {
		t.Fatal(records)
	}
	if accepted, err := s.Observe(ticket, payload(`"flag":{"value":true}`), "session", nil); err != nil || accepted {
		t.Fatalf("disabled observation = %v %v", accepted, err)
	}
	lookup(t, s, ticket, "flag", "false", "payload", capture(&records))
}

func TestFeatureDeferredExposureOrderAndPersistenceBoundary(t *testing.T) {
	entries := `"first":` + feature(`{"a":[1,true]}`, "exp1", `0`) + `,"second":` + feature(`false`, "exp2", `1`)
	store, s, ticket := seeded(t, entries)
	var trace []string
	var records []Exposure
	store.trace = &trace
	sink := func(e Exposure) (bool, error) {
		records = append(records, e)
		trace = append(trace, e.FeatureID)
		return true, nil
	}
	lookup(t, s, ticket, "second", "false", "disk", sink)
	lookup(t, s, ticket, "first", `{"a":[1,true]}`, "disk", sink)
	lookup(t, s, ticket, "second", "false", "disk", sink)
	if len(records) != 0 {
		t.Fatal(records)
	}
	observe(t, s, ticket, payload(entries), sink)
	trace = append(trace, "refreshed")
	if !reflect.DeepEqual(trace, []string{"second", "first", "disk", "refreshed"}) {
		t.Fatal(trace)
	}
	if len(s.pending) != 0 {
		t.Fatal(s.pending)
	}
	if bytes.Contains(store.payloads["account"], []byte("logged")) || bytes.Contains(store.payloads["account"], []byte("session")) {
		t.Fatal("process marks persisted")
	}
	independent := New(store)
	independentTicket := bind(t, independent, "account", "token")
	lookup(t, independent, independentTicket, "first", `{"a":[1,true]}`, "disk", sink)
	observe(t, independent, independentTicket, payload(entries), sink)
	if len(records) != 3 {
		t.Fatalf("host reconstruction inherited dedup: %v", records)
	}
}

func TestFeatureDeferredRejectsChangedMetadataButNotChangedCurrentValue(t *testing.T) {
	for _, tc := range []struct {
		name, entry string
		want        int
	}{
		{"experiment", `"flag":` + feature(`false`, "new", `0`), 0},
		{"variation", `"flag":` + feature(`false`, "exp", `1`), 0},
		{"absent", `"other":{"value":true}`, 0},
		{"value", `"flag":` + feature(`true`, "exp", `0.0`), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, s, ticket := seeded(t, `"flag":`+feature(`false`, "exp", `0`))
			var records []Exposure
			lookup(t, s, ticket, "flag", "false", "disk", nil)
			observe(t, s, ticket, payload(tc.entry), capture(&records))
			if len(records) != tc.want || len(s.pending) != 0 {
				t.Fatalf("records=%v pending=%v", records, s.pending)
			}
		})
	}
}

func TestFeatureDeferredRetryAndOldDiskPolicyDuringDrain(t *testing.T) {
	_, s, ticket := seeded(t, `"flag":`+feature(`false`, "exp", `0`)+`,"tengu_frond_boric":{"value":{"firstParty":true}}`)
	lookup(t, s, ticket, "flag", "false", "disk", nil)
	var records []Exposure
	observe(t, s, ticket, payload(`"flag":`+feature(`false`, "exp", `0`)), capture(&records))
	if len(records) != 0 || !s.logged["flag"] {
		t.Fatalf("old disk policy lost during drain: %v", records)
	}
	_, s, ticket = seeded(t, `"flag":`+feature(`false`, "exp", `0`))
	lookup(t, s, ticket, "flag", "false", "disk", nil)
	observe(t, s, ticket, payload(`"flag":`+feature(`false`, "exp", `0`)), func(Exposure) (bool, error) { return false, nil })
	if len(s.pending) != 0 || len(s.logged) != 0 {
		t.Fatal("refused drain retained marks")
	}
	lookup(t, s, ticket, "flag", "false", "payload", capture(&records))
	if len(records) != 1 {
		t.Fatal(records)
	}
}

func TestFeatureAuthGenerationOwnershipAndReset(t *testing.T) {
	for _, change := range []string{"token", "account"} {
		t.Run(change, func(t *testing.T) {
			_, s, ticket := seeded(t, `"flag":`+feature(`false`, "exp", `0`))
			var records []Exposure
			observe(t, s, ticket, payload(`"flag":`+feature(`false`, "exp", `0`)), nil)
			lookup(t, s, ticket, "flag", "false", "payload", capture(&records))
			scope := "account"
			if change == "account" {
				scope = "new-account"
			}
			current := bind(t, s, scope, "new-token")
			if _, err := s.Observe(ticket, payload(`"flag":{"value":true}`), "old-session", nil); !errors.Is(err, ErrStale) {
				t.Fatal(err)
			}
			if _, err := s.Lookup(ticket, "session", "flag", nil, nil); !errors.Is(err, ErrStale) {
				t.Fatal(err)
			}
			observe(t, s, current, payload(`"flag":`+feature(`false`, "exp", `0`)), nil)
			lookup(t, s, current, "flag", "false", "payload", capture(&records))
			want := 1
			if change == "account" {
				want = 2
			}
			if len(records) != want {
				t.Fatal(records)
			}
			other := New(nil)
			if _, err := other.Observe(current, payload(`"x":{"value":1}`), "session", nil); !errors.Is(err, ErrStale) {
				t.Fatal(err)
			}
		})
	}
	for _, pending := range []bool{false, true} {
		for _, logged := range []bool{false, true} {
			t.Run(fmt.Sprintf("reset/%v/%v", pending, logged), func(t *testing.T) {
				_, s, ticket := seeded(t, `"flag":`+feature(`false`, "exp", `0`))
				lookup(t, s, ticket, "flag", "false", "disk", nil)
				s.logged["other"] = true
				current := s.Reset(pending, logged)
				if (len(s.pending) > 0) != pending || (len(s.logged) > 0) != logged {
					t.Fatal("reset flags not independent")
				}
				if _, err := s.Observe(ticket, payload(`"x":{"value":1}`), "session", nil); !errors.Is(err, ErrStale) {
					t.Fatal(err)
				}
				s.Close()
				if _, err := s.Observe(current, payload(`"x":{"value":1}`), "session", nil); !errors.Is(err, ErrStale) {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestFeatureCorruptPersistenceAndLegacyImport(t *testing.T) {
	for _, p := range []string{`not-json`, `{"version":2}`, `{"version":1,"experiments":{"flag":{"variation":"0","value":false}}}`} {
		t.Run(p, func(t *testing.T) {
			store := &memoryStore{payloads: map[string][]byte{"a": []byte(p)}, revisions: map[string]string{"a": "original"}}
			s := New(store)
			ticket, err := s.Bind("a", "t")
			if err == nil {
				t.Fatal("corrupt cache accepted")
			}
			s.ImportLegacy(ticket, map[string]json.RawMessage{"legacy": json.RawMessage(`1`)})
			accepted, err := s.Observe(ticket, payload(`"flag":{"value":false}`), "session", nil)
			if !accepted || err == nil || store.writes != 0 || string(store.payloads["a"]) != p {
				t.Fatal("corruption overwritten or new facts dropped")
			}
			got, err := s.Lookup(ticket, "session", "flag", nil, nil)
			if string(got.Raw) != "false" || err == nil {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
	store := &memoryStore{saveErr: errors.New("protected write failed")}
	s := New(store)
	ticket := bind(t, s, "a", "t")
	if accepted, err := s.Observe(ticket, payload(`"flag":{"value":false}`), "session", nil); !accepted || err == nil {
		t.Fatal("save failure hidden")
	}
	if _, err := s.Lookup(ticket, "session", "flag", nil, nil); err == nil {
		t.Fatal("save failure forgotten")
	}
	store.saveErr = nil
	observe(t, s, ticket, payload(`"flag":{"value":true}`), nil)
	lookup(t, s, ticket, "flag", "true", "payload", nil)
	legacy := New(nil)
	old := bind(t, legacy, "a", "t")
	values := map[string]json.RawMessage{"flag": json.RawMessage(`false`)}
	legacy.ImportLegacy(old, values)
	values["flag"][0] = 'x'
	lookup(t, legacy, old, "flag", "false", "disk", nil)
	if len(legacy.pending) != 0 {
		t.Fatal("legacy cache invented experiment")
	}
}

func TestFeaturePersistenceHealthSeparatesFetchErrorsAndStaleOwners(t *testing.T) {
	store := &memoryStore{}
	s := New(store)
	ticket := bind(t, s, "account", "token")
	if _, err := s.Observe(ticket, []byte(`invalid`), "session", nil); err == nil {
		t.Fatal("invalid response accepted")
	}
	if err := s.PersistenceError(ticket); err != nil {
		t.Fatal("invalid HTTP JSON became a durable cache failure", err)
	}
	store.saveErr = errors.New("synthetic protected cache failure")
	if _, err := s.Observe(ticket, payload(`"flag":{"value":true}`), "session", nil); err == nil || s.PersistenceError(ticket) == nil {
		t.Fatal("unpersisted evaluation lost its durable failure")
	}
	store.saveErr = nil
	if _, err := s.Lookup(ticket, "session", "flag", nil, nil); err == nil || s.PersistenceError(ticket) == nil {
		t.Fatal("cached read repaired an unpersisted evaluation")
	}
	observe(t, s, ticket, payload(`"flag":{"value":true}`), nil)
	if err := s.PersistenceError(ticket); err != nil {
		t.Fatal("successful write did not repair durable cache health", err)
	}
	next := bind(t, s, "account", "rotated")
	if _, err := s.Observe(ticket, []byte(`invalid`), "session", nil); err != ErrStale {
		t.Fatal("malformed old-token response bypassed generation ownership", err)
	}
	if s.PersistenceError(ticket) != ErrStale || s.PersistenceError(next) != nil {
		t.Fatal("cache health crossed credential generation")
	}
}

func TestFeatureJSONSemanticsAndConcurrentRead(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{`null`, false}, {`false`, false}, {`0`, false}, {`-0`, false}, {`""`, false}, {`[]`, true}, {`{}`, true}, {`"false"`, true}, {`1e400`, true}, {`1 false`, false},
	} {
		if Truthy(json.RawMessage(tc.raw)) != tc.want {
			t.Errorf("truthy %s", tc.raw)
		}
	}
	if !equalJSON(json.RawMessage(`{"b":[1e0,9007199254740993],"a":true}`), json.RawMessage(`{"a":true,"b":[1,9007199254740992]}`)) {
		t.Fatal("comparison is not binary64 structural equality")
	}
	if equalJSON(json.RawMessage(`1 2`), json.RawMessage(`1`)) {
		t.Fatal("invalid JSON compared equal")
	}
	s := New(nil)
	ticket := bind(t, s, "a", "t")
	observe(t, s, ticket, payload(`"flag":`+feature(`false`, "exp", `1e400`)), nil)
	var records []Exposure
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = s.Lookup(ticket, "session", "flag", nil, capture(&records)) }()
	}
	wg.Wait()
	if len(records) != 1 || !math.IsInf(records[0].VariationID, 1) {
		t.Fatal(records)
	}
}
