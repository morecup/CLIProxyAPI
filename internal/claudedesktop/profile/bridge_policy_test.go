package profile

import "testing"

func TestBridgeTeardownBudgetIsVersionedAndValidated(t *testing.T) {
	bundle, err := BuiltinV140609()
	if err != nil {
		t.Fatal(err)
	}
	if bundle.ControlPlane.TeardownArchiveBudgetMillis != 1500 {
		t.Fatal("native default archive budget changed")
	}
	for _, value := range []int{0, 499, 500, 1500, 2000, 2001} {
		bundle.ControlPlane.TeardownArchiveBudgetMillis = value
		valid := value >= 500 && value <= 2000
		if err = bundle.Validate(); (err == nil) != valid {
			t.Fatalf("budget %d: %v", value, err)
		}
	}
}
