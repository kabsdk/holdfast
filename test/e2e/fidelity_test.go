package e2e

// The metadata fixture and comparison live in internal/fidelity, so that lab/fidelity can run them on the
// lab's VMs too. These are the e2e suite's thin wrappers.

import (
	"testing"

	"github.com/kabsdk/holdfast/internal/fidelity"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func buildFixture(t *testing.T, root string) {
	t.Helper()
	must(t, fidelity.Build(root))
}

func scan(t *testing.T, root string) map[string]fidelity.Entry {
	t.Helper()
	m, err := fidelity.Scan(root)
	must(t, err)
	return m
}

func compareTrees(src, dst map[string]fidelity.Entry) []fidelity.Result {
	return fidelity.Compare(src, dst)
}
