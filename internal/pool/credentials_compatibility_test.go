package pool

import (
	"os"
	"testing"
)

// Fixtures were generated from the pre-feature renderer at 1a683a5. Existing
// job definitions must not change merely because the agent binary is upgraded.
func TestLegacyJobRenderingRemainsByteIdentical(t *testing.T) {
	app := App{Name: "legacy-api", Image: "ghcr.io/example/backend:one", Domain: "legacy.example.com", Port: 8080, PreferNode: "oracle-main", CPU: 500, MemoryMB: 512, HealthPath: "/health", Env: map[string]string{"MODE": "production"}}
	for _, tc := range []struct {
		name      string
		fileMount bool
	}{{"public", false}, {"file", true}} {
		t.Run(tc.name, func(t *testing.T) {
			app.SecretEnv = tc.fileMount
			golden, err := os.ReadFile("testdata/legacy-" + tc.name + ".nomad.hcl")
			if err != nil {
				t.Fatal(err)
			}
			if got := renderNomadJob(app); got != string(golden) {
				t.Fatalf("legacy job rendering changed:\n%s", got)
			}
		})
	}
}
