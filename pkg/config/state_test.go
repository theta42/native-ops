package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A service manifest may declare state: running, state: stopped, or nothing
// (absent). Anything else is refused at load.
func TestServiceStateValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state string
		ok    bool
	}{
		{"absent", "", true},
		{"running", "running", true},
		{"stopped", "stopped", true},
		{"bogus", "paused", false},
		{"empty-ish", "STOPPED", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			yml := "name: twenty\nimage: docker:twentycrm/twenty:v2.50.1\n"
			if tc.state != "" {
				yml += "state: " + tc.state + "\n"
			}
			p := filepath.Join(dir, "service.yml")
			if err := os.WriteFile(p, []byte(yml), 0644); err != nil {
				t.Fatal(err)
			}
			svc, err := LoadServiceConfig(p)
			if tc.ok {
				if err != nil {
					t.Fatalf("state %q refused: %v", tc.state, err)
				}
				if svc.State != tc.state {
					t.Errorf("got state %q, want %q", svc.State, tc.state)
				}
				return
			}
			if err == nil {
				t.Fatalf("state %q accepted, want refusal", tc.state)
			}
			if !strings.Contains(err.Error(), "state") {
				t.Errorf("refusal does not name state: %v", err)
			}
		})
	}
}
