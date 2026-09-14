package storage

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcessMemorySamplerPublishesAReading(t *testing.T) {
	sampler := NewProcessMemorySampler(time.Millisecond)

	assert.Zero(t, sampler.Sample(), "a sampler that was never started reports nothing rather than sampling on demand")

	sampler.Start()
	defer sampler.Stop()

	first := sampler.Sample()
	assert.Positive(t, first.HeapAlloc, "Start takes the first reading itself, so INFO is never all zeros")
	assert.Positive(t, first.Goroutines)
	assert.False(t, first.SampledAt.IsZero())

	// The loop keeps it moving. Only the timestamp is guaranteed to change —
	// the heap may legitimately sit still — so that is what is asserted.
	assert.Eventually(t, func() bool {
		return sampler.Sample().SampledAt.After(first.SampledAt)
	}, 2*time.Second, 5*time.Millisecond, "the sampler must refresh on its timer")
}

func TestProcessMemorySamplerLifecycleIsForgiving(t *testing.T) {
	sampler := NewProcessMemorySampler(0)
	assert.Equal(t, DefaultProcessMemoryInterval, sampler.interval, "a non-positive interval means the default")

	t.Run("stop without start does not block", func(t *testing.T) {
		unstarted := NewProcessMemorySampler(time.Hour)
		unstarted.Stop()
		unstarted.Stop()
	})

	t.Run("repeated start and stop are no-ops", func(t *testing.T) {
		sampler.Start()
		sampler.Start()

		sampler.Stop()
		sampler.Stop()

		assert.Positive(t, sampler.Sample().HeapAlloc, "the last reading survives Stop")
	})
}

// TestReadMemStatsIsNotOnARequestPath is the assertion ISSUE-0015 actually
// asks for. The fix is not "ReadMemStats got faster" — it cannot — but "no path
// a client can reach still calls it", and that is a property of where the call
// sits rather than of how long it takes.
//
// So the test reads the package's own source and insists the call appears
// exactly once, inside the sampler's own sample method. Anything else — a
// convenience added to Stats, a lazily-refreshing cache that samples on the
// first request after each interval — puts a stop-the-world back under STATS,
// INFO and the admin API, and fails here.
func TestReadMemStatsIsNotOnARequestPath(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	callers := map[string][]string{}
	fset := token.NewFileSet()

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, parseErr := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		require.NoError(t, parseErr)

		var enclosing string
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.FuncDecl:
				enclosing = n.Name.Name
			case *ast.SelectorExpr:
				pkg, ok := n.X.(*ast.Ident)
				if ok && pkg.Name == "runtime" && n.Sel.Name == "ReadMemStats" {
					callers[name] = append(callers[name], enclosing)
				}
			}
			return true
		})
	}

	assert.Equal(t, map[string][]string{"procmem.go": {"sample"}}, callers,
		"runtime.ReadMemStats stops the world and belongs only in the timer-driven sampler (ISSUE-0015)")
}
