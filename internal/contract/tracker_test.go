package contract

import "testing"

func TestTrackerConfig(t *testing.T) {
	for _, tc := range []struct {
		value, want string
		valid       bool
	}{{"", "github", true}, {"github", "github", true}, {"local", "local", true}, {"LOCAL", "", false}, {"other", "", false}} {
		got, err := ResolveTracker(&SwarmConfig{Tracker: tc.value})
		if got != tc.want || (err == nil) != tc.valid {
			t.Errorf("%q: %q %v", tc.value, got, err)
		}
	}
	for _, raw := range []string{`{}`, `{"tracker":"github"}`, `{"tracker":"local"}`} {
		if err := ValidateConfigJSON([]byte(raw)); err != nil {
			t.Error(err)
		}
	}
	for _, raw := range []string{`{"tracker":"other"}`, `{"tracker":2}`, `{"tracker":true}`, `{"tracker":null}`, `{"tracker":[]}`, `{"tracker":""}`} {
		if err := ValidateConfigJSON([]byte(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}
