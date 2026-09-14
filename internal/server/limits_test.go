package server

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/b3vet/atlascache/internal/protocol"
)

// TestConnLimitsNormalizeFillsWhatWasNotSet. A partly filled ConnLimits is what
// a caller that cares about one bound writes, and every field it left alone
// must come back as a limit rather than as no limit at all.
func TestConnLimitsNormalizeFillsWhatWasNotSet(t *testing.T) {
	got := ConnLimits{MaxConnections: 5}.normalize()

	assert.Equal(t, 5, got.MaxConnections)
	assert.Equal(t, defaultMaxRequestBytes, got.MaxRequestBytes)
	assert.Equal(t, defaultMaxPipelineCommands, got.MaxPipelineCommands)

	// The two that mean something by being zero are covered separately; a
	// negative value is the unusable one, and that is what gets replaced.
	assert.Equal(t, defaultIdleTimeout, ConnLimits{IdleTimeout: -1}.normalize().IdleTimeout)
	assert.Equal(t, defaultMaxOutputBytes, ConnLimits{MaxOutputBytes: -1}.normalize().MaxOutputBytes)
}

// TestNormalizeKeepsTheDeliberateZeros. Two fields have a meaning for zero, and
// treating them as unset would switch a limit back on that an operator turned
// off.
func TestNormalizeKeepsTheDeliberateZeros(t *testing.T) {
	got := ConnLimits{IdleTimeout: 0, MaxOutputBytes: 0}.normalize()

	assert.Zero(t, got.IdleTimeout, "0 disables the idle timeout")
	assert.Zero(t, got.MaxOutputBytes, "0 leaves output uncapped")
}

// TestTheRequestBudgetHasAFloor. The budget is charged against bytes drawn from
// the socket, and one of those reads may carry a whole pipeline batch; a budget
// at or below a read buffer would refuse the first request of a batch for
// arriving alongside others.
func TestTheRequestBudgetHasAFloor(t *testing.T) {
	assert.Equal(t, minRequestBytes, ConnLimits{MaxRequestBytes: 1}.normalize().MaxRequestBytes)
	assert.Greater(t, minRequestBytes, connBufferSize)
}

// TestCodecLimitsTieTheParserToBothCeilings. The bulk limit comes from the
// engine's value size and the element limit from the request budget, so neither
// pair can drift: ISSUE-0016 was the first drifting apart and ISSUE-0018 was the
// second never having been tied together at all.
func TestCodecLimitsTieTheParserToBothCeilings(t *testing.T) {
	t.Run("the bulk limit follows max_value_size", func(t *testing.T) {
		limits := DefaultConnLimits().codecLimits(64 * 1024)
		assert.Equal(t, protocol.LimitsForValueSize(64*1024).MaxBulkLength, limits.MaxBulkLength)
	})

	t.Run("the element cap is below the protocol ceiling", func(t *testing.T) {
		limits := DefaultConnLimits().codecLimits(1 << 20)
		assert.Equal(t, maxRequestElements, limits.MaxMultiBulkLength)
		assert.Less(t, limits.MaxMultiBulkLength, protocol.DefaultLimits().MaxMultiBulkLength,
			"a million elements is 154MB to decode (ISSUE-0018)")
	})

	t.Run("a tighter budget tightens the element cap with it", func(t *testing.T) {
		budget := 256 * 1024
		limits := ConnLimits{MaxRequestBytes: budget}.normalize().codecLimits(1 << 20)
		assert.Equal(t, budget/minElementWire, limits.MaxMultiBulkLength)
	})

	t.Run("a generous budget does not reopen the ceiling", func(t *testing.T) {
		limits := ConnLimits{MaxRequestBytes: 64 << 20}.normalize().codecLimits(16 << 20)
		assert.Equal(t, maxRequestElements, limits.MaxMultiBulkLength)
	})
}

// TestTheElementCapCostsWhatItClaims is the arithmetic the cap was chosen from,
// pinned so that a later change to either number is a decision rather than an
// accident. 147 bytes an element is what
// TestOneAcceptedRequestHasABoundedCost measures in internal/protocol.
func TestTheElementCapCostsWhatItClaims(t *testing.T) {
	const bytesPerElement = 147

	assert.Less(t, maxRequestElements*bytesPerElement, 32<<20,
		"the element cap allows %d bytes of decoding per request", maxRequestElements*bytesPerElement)
}

// TestDefaultConnLimitsAreTheOnesDocumented keeps the three places the defaults
// appear — here, internal/config, and config.example.yaml — from drifting into
// disagreement.
func TestDefaultConnLimitsAreTheOnesDocumented(t *testing.T) {
	limits := DefaultConnLimits()

	assert.Equal(t, 10000, limits.MaxConnections)
	assert.Equal(t, 30*time.Second, limits.IdleTimeout)
	assert.Equal(t, 1024, limits.MaxPipelineCommands)
	assert.Equal(t, 64<<20, limits.MaxOutputBytes)
}
