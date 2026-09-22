package features

import (
	"encoding/json"
	"sync"
	"testing"
)

func TestSDKHostsSeparateExposuresButSerializeTheirAcceptedCache(t *testing.T) {
	base := &memoryStore{}
	shared := NewSharedStore(base)
	a, b := NewHost(shared.View(), "same-label"), NewHost(shared.View(), "same-label")
	defer a.Close()
	defer b.Close()
	ta, tb := bind(t, a.Service(), "account", "token"), bind(t, b.Service(), "account", "token")
	p := payload(`"flag":` + feature("true", "experiment", "0"))
	observe(t, a.Service(), ta, p, nil)
	observe(t, b.Service(), tb, p, nil)
	var events []Exposure
	sink := func(e Exposure) (bool, error) { events = append(events, e); return true, nil }
	for _, host := range []*Host{a, a, b, b} {
		ticket := bind(t, host.Service(), "account", "token")
		if _, err := host.Service().Lookup(ticket, host.SessionID(), "flag", json.RawMessage(`false`), sink); err != nil {
			t.Fatal(err)
		}
	}
	if len(events) != 2 || base.writes != 2 {
		t.Fatalf("events=%d writes=%d", len(events), base.writes)
	}
	if err := a.Reidentify("resumed"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Service().Lookup(ta, a.SessionID(), "flag", nil, sink); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatal("identity change reset the host's exposure set")
	}
}

func TestSDKHostCloseInvalidatesOnlyItsOwnOutstandingObservation(t *testing.T) {
	shared := NewSharedStore(nil)
	a, b := NewHost(shared.View(), "a"), NewHost(shared.View(), "b")
	defer b.Close()
	ta, tb := bind(t, a.Service(), "scope", "token"), bind(t, b.Service(), "scope", "token")
	a.Close()
	p := payload(`"flag":{"value":true}`)
	if _, err := a.Service().Observe(ta, p, a.SessionID(), nil); err != ErrStale {
		t.Fatal(err)
	}
	if _, err := b.Service().Observe(tb, p, b.SessionID(), nil); err != nil {
		t.Fatal(err)
	}
	if a.Context().Err() == nil || b.Context().Err() != nil {
		t.Fatal("host cancellation leaked or was lost")
	}
}

func TestSharedFeatureCacheRejectsExternalWriterAndStaleView(t *testing.T) {
	base := &memoryStore{}
	shared := NewSharedStore(base)
	a, b := shared.View(), shared.View()
	_, ar, _ := a.Load("scope")
	_, br, _ := b.Load("scope")
	if _, err := a.Save("scope", ar, []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Save("scope", ar, []byte(`{"stale":true}`)); err == nil {
		t.Fatal("stale view was accepted")
	}
	if _, err := b.Save("scope", br, []byte(`{"b":2}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := base.Save("scope", base.revisions["scope"], []byte(`{"external":true}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := shared.View().Load("scope"); err == nil {
		t.Fatal("external revision authorized older writers")
	}
	if _, err := b.Save("scope", base.revisions["scope"], nil); err == nil {
		t.Fatal("external revision forged a view")
	}
}

func TestSharedFeatureCacheConcurrentHostsRetainIndependentViews(t *testing.T) {
	shared := NewSharedStore(&memoryStore{})
	views := make([]Store, 12)
	for i := range views {
		views[i] = shared.View()
		if _, _, err := views[i].Load("scope"); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for _, view := range views {
		wg.Go(func() {
			if _, err := view.Save("scope", "", []byte(`{"value":true}`)); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}

func TestSDKHostRegistryWarmAdoptionReidentityAndReplacement(t *testing.T) {
	r := NewRegistry(nil)
	defer r.Close()
	warm := r.Warm()
	a, err := r.Main("first-root", "first-session")
	if err != nil || a != warm || a.SessionID() != "first-session" {
		t.Fatal(err, a)
	}
	a2, err := r.Main("first-root", "first-session")
	if err != nil || a2 != a {
		t.Fatal("another main turn replaced the query")
	}
	b, err := r.Main("second-root", "first-session")
	if err != nil || b == a {
		t.Fatal("separate root inherited an existing host")
	}
	if err := r.Reidentify(a, "first-root", "resumed-root", "resumed-session"); err != nil {
		t.Fatal(err)
	}
	if r.Lookup("first-root") != nil || r.Lookup("resumed-root") != a || a.SessionID() != "resumed-session" {
		t.Fatal("root identity change replaced the host")
	}
	if err := r.Reidentify(a, "resumed-root", "second-root", "collision"); err == nil {
		t.Fatal("existing root was overwritten")
	}
	next, err := r.Replace(a, "resumed-root", "resumed-session")
	if err != nil || next == a || a.Context().Err() == nil || next.Context().Err() != nil {
		t.Fatal("explicit new query did not replace the generation", err)
	}
	if _, err := r.Replace(a, "resumed-root", "late"); err == nil {
		t.Fatal("stale query replaced a newer one")
	}
	r.Close()
	if next.Context().Err() == nil || b.Context().Err() == nil {
		t.Fatal("runtime close left live hosts")
	}
	if _, err := r.Main("late", "late"); err == nil {
		t.Fatal("closed runtime was resurrected")
	}
}

func TestUnownedFeaturePeekCannotAddDeferredOrLoggedExposures(t *testing.T) {
	s := New(nil)
	ticket := bind(t, s, "scope", "token")
	p := payload(`"flag":` + feature("true", "experiment", "0"))
	observe(t, s, ticket, p, nil)
	if value, err := s.Peek(ticket, "flag", json.RawMessage(`false`)); err != nil || !Truthy(value.Raw) {
		t.Fatal(value, err)
	}
	if len(s.logged) != 0 || len(s.pending) != 0 {
		t.Fatal("unowned feature access polluted a native host")
	}
}

func TestClosedSDKHostRejectsMalformedOutstandingObservationAsStale(t *testing.T) {
	host := NewHost(nil, "session")
	ticket := bind(t, host.Service(), "scope", "token")
	host.Close()
	if _, err := host.Service().Observe(ticket, []byte(`invalid`), "session", nil); err != ErrStale {
		t.Fatal("closed host processed a response before checking its owner", err)
	}
}

func TestSDKHostHealthIdentityIsNotTheSession(t *testing.T) {
	a, b := NewHost(nil, "shared-transcript"), NewHost(nil, "shared-transcript")
	defer a.Close()
	defer b.Close()
	id := a.ID()
	if id == "" || id == b.ID() || id == a.SessionID() {
		t.Fatal("query health identity reused a transcript or another query")
	}
	if err := a.Reidentify("resumed-transcript"); err != nil {
		t.Fatal(err)
	}
	if a.ID() != id {
		t.Fatal("session reidentity changed query health owner")
	}
}

func TestSDKHostRejectedBetaStateIsLiveAndSessionScoped(t *testing.T) {
	host := NewHost(nil, "first-session")
	defer host.Close()
	const beta = "fallback-credit-2026-06-01"
	if host.BetaRejectedForSession("first-session", beta) || host.RejectBetaForSession("", beta) || host.RejectBetaForSession("first-session", "") {
		t.Fatal("fresh or empty rejection was invented")
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if !host.RejectBetaForSession("first-session", beta) || !host.BetaRejectedForSession("first-session", beta) {
				t.Error("live host lost a concurrent rejection")
			}
		})
	}
	wg.Wait()
	if err := host.Reidentify("second-session"); err != nil {
		t.Fatal(err)
	}
	if host.BetaRejectedForSession("first-session", beta) || !host.BetaRejectedForSession("second-session", beta) || host.RejectBetaForSession("first-session", "late-beta") {
		t.Fatal("reidentification lost live state or admitted a stale response")
	}
	fresh := NewHost(nil, "second-session")
	defer fresh.Close()
	if fresh.BetaRejectedForSession("second-session", beta) {
		t.Fatal("replacement host borrowed another instance's rejection")
	}
	host.Close()
	if host.RejectBetaForSession("second-session", "another-beta") || host.BetaRejectedForSession("second-session", beta) {
		t.Fatal("closed host still authorized beta decisions")
	}
}

func TestClosedQueryRestartsOnceOnNextMainWithoutReidentifyingTranscript(t *testing.T) {
	r := NewRegistry(nil)
	defer r.Close()
	first, err := r.ResolveMain("caller")
	if err != nil {
		t.Fatal(err)
	}
	first.Close()
	if session, err := r.Session("caller"); err != nil || session != first.SessionID() {
		t.Fatal("helper alias lookup lost the closed query's transcript", session, err)
	}
	var mu sync.Mutex
	var next *Host
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			host, err := r.ResolveMain("caller")
			mu.Lock()
			defer mu.Unlock()
			if err != nil || host == first || host.ID() == first.ID() || host.SessionID() != first.SessionID() || host.Context().Err() != nil {
				t.Error("next main resurrected a closed query or lost its transcript", err)
			}
			if next != nil && next != host {
				t.Error("concurrent main created duplicate replacement queries")
			}
			next = host
		})
	}
	wg.Wait()
	if r.LookupSession(first.SessionID()) != next {
		t.Fatal("closed host remained the current transcript owner")
	}
}
