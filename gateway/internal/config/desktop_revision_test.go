package config

import "testing"

func TestDesktopRevisionIdentifiesOrderedTargets(t *testing.T) {
	targets := map[string]string{"alpha": "example/Alpha", "beta": "example/Beta"}
	base := DesktopModelRevision([]string{"alpha", "beta"}, targets)
	for _, aliases := range [][]string{{"alpha"}, {"alpha", "beta", "gamma"}, {"beta", "alpha"}} {
		if DesktopModelRevision(aliases, targets) == base {
			t.Fatal("changed selection reused revision")
		}
	}
	targets["beta"] = "example/Other"
	if DesktopModelRevision([]string{"alpha", "beta"}, targets) == base {
		t.Fatal("same-count target change reused revision")
	}
}
