package telemetry

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSDKFeatureHostObserverFailureDegradesEndpointWithoutWorker(t *testing.T) {
	for _, failure := range []string{"binding", "worker-directory"} {
		t.Run(failure, func(t *testing.T) {
			m := newTelemetryTestManager(t, t.TempDir(), &testClock{now: time.Now()}, &testDoer{}, nil)
			auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
			if failure == "binding" {
				auth = nil
			} else {
				binding, err := m.bindingForDelivery(auth, m.sdkDelivery)
				if err != nil {
					t.Fatal(err)
				}
				directory := telemetryWorkerDirectory(m.root, m.sdkDelivery, binding.BindingRevision)
				if err := os.MkdirAll(filepath.Dir(directory), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(directory, []byte("synthetic directory conflict"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			observe, retire, err := m.SDKFeatureHostObserver(auth, "query")
			if err == nil || observe != nil || retire != nil {
				t.Fatal("unavailable health worker returned a usable observer")
			}
			status := m.Status()
			for _, endpoint := range status.DeliveryEndpoints {
				if endpoint.Role == m.sdkDelivery.endpointRole {
					if endpoint.Status != "awaiting-sdk-feature-facts" || endpoint.Reason != "SDK query health observer is unavailable" {
						t.Fatal("missing health worker still reported a ready SDK endpoint", endpoint)
					}
					return
				}
			}
			t.Fatal("SDK endpoint disappeared from management status")
		})
	}
}

func TestSDKFeatureHostHealthRetiresOnlyItsLiveFault(t *testing.T) {
	root := t.TempDir()
	clock := &testClock{now: time.Now()}
	m := newTelemetryTestManager(t, root, clock, &testDoer{}, nil)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	a, retireA, err := m.SDKFeatureHostObserver(auth, "query-a")
	if err != nil {
		t.Fatal(err)
	}
	b, retireB, err := m.SDKFeatureHostObserver(auth, "query-b")
	if err != nil {
		t.Fatal(err)
	}
	defer retireB()
	worker, err := m.workerForDelivery(auth, m.sdkDelivery)
	if err != nil {
		t.Fatal(err)
	}
	check := func(want int) {
		t.Helper()
		issue := worker.factIssueSnapshot()
		if (want == 0 && issue != nil) || (want != 0 && (issue == nil || issue.Unresolved != want || issue.Status != "awaiting-sdk-feature-facts")) {
			t.Fatalf("want %d unresolved SDK feature faults, got %+v", want, issue)
		}
	}
	if err := m.ObserveSDKFeatureState(auth, "feature-cache:durable", false); err != nil {
		t.Fatal(err)
	}
	if err := worker.setFactIssue(factIssueSDKExposure, "session", "feature", true); err != nil {
		t.Fatal(err)
	}
	_ = a(false)
	_ = b(false)
	check(4)
	_ = b(true)
	check(3)
	retireA()
	_ = a(false)
	check(2)
	_ = b(false)
	check(3)
	// Rebuilding the process retires only the live query fault. Its unresolved
	// protected-cache and undelivered-exposure evidence must be restored.
	m.Close()
	restored := newTelemetryTestManager(t, root, clock, &testDoer{}, nil)
	worker, err = restored.workerForDelivery(auth, restored.sdkDelivery)
	if err != nil {
		t.Fatal(err)
	}
	check(2)
	retireB()
	_ = b(true)
	check(2)
	if err := restored.ObserveSDKFeatureState(auth, "feature-cache:durable", true); err != nil {
		t.Fatal(err)
	}
	check(1)
	if err := worker.setFactIssue(factIssueSDKExposure, "session", "feature", false); err != nil {
		t.Fatal(err)
	}
	check(0)
}

func TestSDKFeatureHostRetirementWinsConcurrentLateCallbacks(t *testing.T) {
	m := newTelemetryTestManager(t, t.TempDir(), &testClock{now: time.Now()}, &testDoer{}, nil)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	observe, retire, err := m.SDKFeatureHostObserver(auth, "query")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			for range 20 {
				_ = observe(false)
			}
		})
	}
	retire()
	wg.Wait()
	worker, err := m.workerForDelivery(auth, m.sdkDelivery)
	if err != nil || worker.factIssueSnapshot() != nil {
		t.Fatal("late callback resurrected a retired health owner", err)
	}
}

func TestSDKFeatureHostHealthIsAccountBound(t *testing.T) {
	m := newTelemetryTestManager(t, t.TempDir(), &testClock{now: time.Now()}, &testDoer{}, nil)
	authA := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	authB := newTelemetryTestAuth(t, testAccountB, testOrgB, testDeviceB)
	a, retireA, err := m.SDKFeatureHostObserver(authA, "same-local-label")
	if err != nil {
		t.Fatal(err)
	}
	defer retireA()
	b, retireB, err := m.SDKFeatureHostObserver(authB, "same-local-label")
	if err != nil {
		t.Fatal(err)
	}
	defer retireB()
	_ = a(false)
	_ = b(true)
	workerA, _ := m.workerForDelivery(authA, m.sdkDelivery)
	workerB, _ := m.workerForDelivery(authB, m.sdkDelivery)
	if workerA.factIssueSnapshot() == nil || workerB.factIssueSnapshot() != nil {
		t.Fatal("another account cleared a query's fault")
	}
	retireA()
	_ = b(false)
	if workerA.factIssueSnapshot() != nil || workerB.factIssueSnapshot() == nil {
		t.Fatal("retirement crossed an account binding")
	}
}
