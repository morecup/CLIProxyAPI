package features

import (
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestSDKHostAliasAdoptsInitialEvaluationIdentityAndRestoresOnlySession(t *testing.T) {
	aliases := &memoryStore{}
	r := NewRegistry(nil, aliases)
	warm := r.Warm()
	initial := warm.SessionID()
	if session, err := r.Session("first-caller"); err != nil || session != "" || r.claimed {
		t.Fatal("an unknown auxiliary lookup claimed the warm query", err)
	}
	a, err := r.ResolveMain("first-caller")
	if err != nil || a != warm || a.SessionID() != initial {
		t.Fatal("fresh main detached from its initial evaluation", err)
	}
	if again, err := r.ResolveMain("first-caller"); err != nil || again != a || aliases.writes != 1 {
		t.Fatal("another main request allocated or persisted a new owner", err)
	}
	b, err := r.ResolveMain("second-caller")
	if err != nil || b == a || b.SessionID() == initial {
		t.Fatal("independent roots share a native session", err)
	}
	ticket := bind(t, a.Service(), "account", "token")
	observe(t, a.Service(), ticket, payload(`"flag":`+feature("true", "experiment", "0")), nil)
	_, _ = a.Service().Lookup(ticket, initial, "flag", nil, func(Exposure) (bool, error) { return true, nil })
	r.Close()
	next := NewRegistry(nil, aliases)
	defer next.Close()
	if session, err := next.Session("first-caller"); err != nil || session != initial || next.claimed {
		t.Fatal("a resumed helper created a new query", err)
	}
	restored, err := next.ResolveMain("first-caller")
	if err != nil || restored == a || restored.SessionID() != initial || len(restored.Service().logged) != 0 || aliases.writes != 2 {
		t.Fatal("restart lost the transcript identity or restored process-local exposure marks", err)
	}
}

func TestSDKHostAliasesSerializeConcurrentFreshMainClaims(t *testing.T) {
	r := NewRegistry(nil, &memoryStore{})
	defer r.Close()
	var wg sync.WaitGroup
	var mu sync.Mutex
	hosts := map[*Host]bool{}
	for range 24 {
		wg.Go(func() {
			host, err := r.ResolveMain("same-caller")
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			hosts[host] = true
			mu.Unlock()
		})
	}
	wg.Wait()
	if len(hosts) != 1 || !hosts[r.Warm()] {
		t.Fatal("concurrent turns allocated multiple SDK hosts")
	}
}

func TestSDKHostQueriesShareOnlyDurableTranscriptAlias(t *testing.T) {
	for _, persisted := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "durable"}[persisted], func(t *testing.T) {
			var aliases Store
			if persisted {
				aliases = &memoryStore{}
			}
			r := NewRegistry(nil, aliases)
			defer r.Close()
			a, err := r.ResolveQuery("connection-a", "caller")
			if err != nil {
				t.Fatal(err)
			}
			b, err := r.ResolveQuery("connection-b", "caller")
			if err != nil || a == b || a.SessionID() != b.SessionID() {
				t.Fatal("query and transcript ownership were conflated", err)
			}
			if got, err := r.FindSession(a.SessionID()); got != nil || !errors.Is(err, ErrAmbiguousSession) {
				t.Fatal("shared transcript was not ambiguous", err)
			}
			if again, err := r.ResolveQuery("connection-a", "caller"); err != nil || again != a {
				t.Fatal("same connection lost query state", err)
			}
			if _, err := r.ResolveQuery("connection-a", "another-caller"); err == nil {
				t.Fatal("query alias changed implicitly")
			}
			a.Close()
			c, err := r.ResolveQuery("connection-c", "caller")
			if err != nil || c == a || c == b || c.SessionID() != a.SessionID() || b.Context().Err() != nil {
				t.Fatal("new connection lost history or retired its sibling", err)
			}
			if id, err := r.QuerySession("unknown-helper", "caller"); err != nil || id != b.SessionID() || r.Lookup("unknown-helper") != nil {
				t.Fatal("helper created a query", err)
			}
			if persisted {
				next := NewRegistry(nil, aliases)
				defer next.Close()
				resumed, err := next.ResolveQuery("new-process", "caller")
				if err != nil || resumed.SessionID() != a.SessionID() {
					t.Fatal("durable alias was saved under the connection key", err)
				}
			}
		})
	}
}

func TestSDKHostSplitQueryReplacementCheckpointsAliasNotConnection(t *testing.T) {
	aliases := &memoryStore{}
	r := NewRegistry(nil, aliases)
	defer r.Close()
	a, _ := r.ResolveQuery("connection", "caller")
	sibling, _ := r.ResolveQuery("sibling", "caller")
	nextID := uuid.NewString()
	next, err := r.Replace(a, "connection", nextID)
	if err != nil || next.SessionID() != nextID || sibling.Context().Err() != nil {
		t.Fatal(err)
	}
	r.Retire(a)
	if r.Lookup("connection") != next || next.Context().Err() != nil {
		t.Fatal("retirement of the old generation closed its replacement")
	}
	restored := NewRegistry(nil, aliases)
	defer restored.Close()
	if session, err := restored.Session("caller"); err != nil || session != nextID {
		t.Fatal("replacement checkpointed wrong alias", err)
	}
	if session, err := restored.Session("connection"); err != nil || session != "" {
		t.Fatal("connection leaked into durable alias namespace", err)
	}
	if err := r.ReidentifyQuery(next, "connection", "renamed-query", "caller", nextID); err != nil {
		t.Fatal(err)
	}
	r.Retire(next)
	if r.Lookup("renamed-query") != nil || next.Context().Err() == nil || sibling.Context().Err() != nil {
		t.Fatal("exact-generation retirement missed reidentity or affected its sibling")
	}
}

func TestSDKHostSessionLookupDistinguishesAmbiguityFromAbsence(t *testing.T) {
	r := NewRegistry(nil)
	defer r.Close()
	if host, err := r.FindSession("unknown"); host != nil || err != nil {
		t.Fatal(host, err)
	}
	a, _ := r.Main("a", "shared")
	b, _ := r.Main("b", "shared")
	if host, err := r.FindSession("shared"); host != nil || !errors.Is(err, ErrAmbiguousSession) {
		t.Fatal("ambiguous transcript looked like a missing query", host, err)
	}
	b.Close()
	if host, err := r.FindSession("shared"); host != a || err != nil {
		t.Fatal("closed query remained an ambiguous live owner", host, err)
	}
}

func TestSDKHostAliasFailureNeverOverwritesEvidenceOrRejectsLiveOwner(t *testing.T) {
	for _, stage := range []string{"load", "save", "corrupt"} {
		t.Run(stage, func(t *testing.T) {
			base := &memoryStore{}
			sentinel := errors.New("synthetic storage failure")
			switch stage {
			case "load":
				base.loadErr = sentinel
			case "save":
				base.saveErr = sentinel
			case "corrupt":
				_, _ = base.Save("caller", "", []byte(`{"version":1,"session_id":"invalid"}`))
			}
			writes := base.writes
			r := NewRegistry(nil, base)
			defer r.Close()
			a, err := r.ResolveMain("caller")
			if a == nil || err == nil || a != r.Warm() || base.writes != writes {
				t.Fatal("lost live inference ownership, hid failure, or overwrote damaged state", err)
			}
			if again, err := r.ResolveMain("caller"); again != a || err == nil {
				t.Fatal("repeated request hid the unresolved persistence failure", err)
			}
			a.Close()
			restarted, err := r.ResolveMain("caller")
			if restarted == nil || restarted == a || restarted.SessionID() != a.SessionID() || err == nil || base.writes != writes {
				t.Fatal("query restart lost alias failure or overwrote its evidence", err)
			}
		})
	}
}

func TestSDKHostAliasMigrationPreservesProvenPriorSession(t *testing.T) {
	r := NewRegistry(nil, &memoryStore{})
	defer r.Close()
	prior := uuid.NewString()
	host, err := r.ResolveMain("legacy-caller", prior)
	if err != nil || host.SessionID() != prior {
		t.Fatal("migration stranded the proven prior transcript", err)
	}
	if again, err := r.ResolveMain("legacy-caller", uuid.NewString()); err != nil || again != host || again.SessionID() != prior {
		t.Fatal("later legacy candidate replaced an established alias", err)
	}
}

func TestSDKHostAliasTransitionsCheckpointBeforeChangingLiveOwnership(t *testing.T) {
	aliases := &memoryStore{}
	r := NewRegistry(nil, aliases)
	defer r.Close()
	first, err := r.ResolveMain("caller")
	if err != nil {
		t.Fatal(err)
	}
	initial, resumed := first.SessionID(), uuid.NewString()
	aliases.saveErr = errors.New("synthetic alias write failure")
	if err := r.Reidentify(first, "caller", "resumed-caller", resumed); err == nil || first.SessionID() != initial || r.Lookup("caller") != first {
		t.Fatal("failed reidentity changed the live owner", err)
	}
	if _, err := r.Replace(first, "caller", resumed); err == nil || first.Context().Err() != nil || r.Lookup("caller") != first {
		t.Fatal("failed replacement retired the only live owner", err)
	}
	aliases.saveErr = nil
	if err := r.Reidentify(first, "caller", "resumed-caller", resumed); err != nil {
		t.Fatal(err)
	}
	nextSession := uuid.NewString()
	next, err := r.Replace(first, "resumed-caller", nextSession)
	if err != nil || next == first || first.Context().Err() == nil {
		t.Fatal("replacement did not establish a new process generation", err)
	}
	reconstructed := NewRegistry(nil, aliases)
	defer reconstructed.Close()
	if got, err := reconstructed.Session("caller"); err != nil || got != initial {
		t.Fatal("moving the live root destroyed its prior transcript alias", err)
	}
	if got, err := reconstructed.Session("resumed-caller"); err != nil || got != nextSession {
		t.Fatal("the newest explicit identity was not durable", err)
	}
}
