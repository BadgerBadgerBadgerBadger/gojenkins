package gojenkins

import (
	"os"
	"testing"
)

// integrationEnv, when set, runs the tests that need a live Jenkins on
// localhost:8080 (as upstream gates them since b4bae26).
const integrationEnv = "INTEGRATION"

func integrationEnabled() bool {
	_, ok := os.LookupEnv(integrationEnv)
	return ok
}

func requireIntegration(t *testing.T) {
	t.Helper()
	if !integrationEnabled() {
		t.Skipf("needs a live Jenkins; set %s to run", integrationEnv)
	}
}
