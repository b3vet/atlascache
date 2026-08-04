// Package eviction implements AtlasCache's victim selection: which keys a
// write drops when it needs room under max_memory.
//
// # Sampling, not ordering
//
// Selection is approximate by design (ADR-0012). A policy draws sample_size
// entries from the shard the write is bound for, ranks them, and returns the
// worst. There is no linked list and no per-entry bookkeeping, so an entry costs
// no more than it did before eviction existed, and — the reason this matters —
// a read stays a read: nothing here asks for the write lock a true LRU ordering
// would need on every GET.
//
// The trade is that the victim is the worst of a sample rather than the worst in
// the shard. Redis has run allkeys-lru this way for a decade.
//
// # Ranking
//
//	lru    Entry.LastAccess ascending    least recently read goes first
//	lfu    Entry.AccessCnt, age-discounted
//	fifo   Entry.CreatedAt ascending     insertion order
//	none   evicts nothing; the write gets ErrOutOfMemory
//
// # Decay without scanning
//
// A raw frequency counter never forgets, so a key that was hot last week
// outranks one that is hot now, forever. The counter is therefore discounted at
// sample time by how long the entry has gone untouched, halving once per
// half-life, computed from the LastAccess the entry already carries. Nothing
// walks the keyspace to age counters — that would reintroduce exactly the O(n)
// cost ISSUE-0012 removed from Stats().
package eviction

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/b3vet/atlascache/internal/storage"
)

// Policy names, as they appear in eviction.policy.
const (
	PolicyLRU  = "lru"
	PolicyLFU  = "lfu"
	PolicyFIFO = "fifo"
	PolicyNone = "none"
)

const (
	// DefaultSampleSize is the number of entries drawn per selection round.
	DefaultSampleSize = 5

	// MaxSampleSize caps the sample. Past this the scan and the sort cost more
	// than the extra accuracy is worth on a write path.
	MaxSampleSize = 100

	// DefaultLFUHalfLife is how long an entry goes untouched before its
	// frequency counts for half as much.
	DefaultLFUHalfLife = time.Minute

	// maxHalvings bounds the discount shift. Beyond this the counter has been
	// halved into nothing anyway, and the shift would be undefined.
	maxHalvings = 62
)

// ErrUnknownPolicy is returned for a policy name that is not one of the four.
var ErrUnknownPolicy = errors.New("eviction: unknown policy")

// Controller selects victims under the configured policy.
//
// Implementations are safe for concurrent use, including SetPolicy: the policy
// is switchable at runtime through config hot-reload, which happens while
// writes are in flight.
type Controller interface {
	// SelectVictims returns keys to drop from shard, best candidate first,
	// covering at least needed bytes where the sample allows it. An empty
	// result means there was nothing to take — no live entries, or a policy
	// that evicts nothing — and the caller must not ask again in a loop.
	SelectVictims(shard *storage.Shard, needed uint64) []string

	// Policy returns the active policy name.
	Policy() string

	// SetPolicy switches policy, returning ErrUnknownPolicy for a name that is
	// not one of the four. The switch takes effect on the next selection.
	SetPolicy(name string) error
}

// Config tunes the controller. The zero value is valid: it yields lru with the
// default sample size.
type Config struct {
	// Policy is one of lru, lfu, fifo, none. Empty means lru.
	Policy string

	// SampleSize is the number of entries drawn per round. Zero means
	// DefaultSampleSize; values above MaxSampleSize are clamped.
	SampleSize int

	// LFUHalfLife is how long an idle entry's frequency takes to count for
	// half as much. Zero means DefaultLFUHalfLife. It has no effect on the
	// other policies.
	LFUHalfLife time.Duration
}

var _ Controller = (*controller)(nil)

// controller is the Controller implementation. The active policy lives in an
// atomic pointer so a reload can swap it without locking the write path.
type controller struct {
	sampleSize int
	halfLife   int64

	policy atomic.Pointer[policy]
}

// policy is a named ranking. A nil score means the policy evicts nothing.
type policy struct {
	name  string
	score func(entry *storage.Entry, now int64) int64
}

// candidate is one sampled entry, reduced to what ranking needs.
type candidate struct {
	key   string
	size  uint64
	score int64
}

// New returns a Controller for cfg, or ErrUnknownPolicy if the policy name is
// not one of the four.
func New(cfg Config) (Controller, error) {
	sampleSize := cfg.SampleSize
	switch {
	case sampleSize <= 0:
		sampleSize = DefaultSampleSize
	case sampleSize > MaxSampleSize:
		sampleSize = MaxSampleSize
	}

	halfLife := cfg.LFUHalfLife
	if halfLife <= 0 {
		halfLife = DefaultLFUHalfLife
	}

	c := &controller{
		sampleSize: sampleSize,
		halfLife:   int64(halfLife),
	}
	if err := c.SetPolicy(cfg.Policy); err != nil {
		return nil, err
	}

	return c, nil
}

// Policy returns the active policy name. See Controller.Policy.
func (c *controller) Policy() string {
	return c.policy.Load().name
}

// SetPolicy switches policy. See Controller.SetPolicy.
func (c *controller) SetPolicy(name string) error {
	p, err := c.policyFor(name)
	if err != nil {
		return err
	}

	c.policy.Store(p)

	return nil
}

// policyFor resolves a policy name to its ranking.
func (c *controller) policyFor(name string) (*policy, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", PolicyLRU:
		return &policy{name: PolicyLRU, score: scoreLRU}, nil
	case PolicyLFU:
		return &policy{name: PolicyLFU, score: c.scoreLFU}, nil
	case PolicyFIFO:
		return &policy{name: PolicyFIFO, score: scoreFIFO}, nil
	case PolicyNone:
		return &policy{name: PolicyNone}, nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownPolicy, name)
	}
}

// SelectVictims samples the shard and ranks it. See Controller.SelectVictims.
func (c *controller) SelectVictims(shard *storage.Shard, needed uint64) []string {
	active := c.policy.Load()
	if shard == nil || needed == 0 || active.score == nil {
		return nil
	}

	samples := shard.Sample(c.sampleSize)
	if len(samples) == 0 {
		// A shard with nothing live in it. Returning empty is what stops the
		// caller from spinning.
		return nil
	}

	now := time.Now().UnixNano()
	ranked := make([]candidate, 0, len(samples))
	for _, entry := range samples {
		ranked = append(ranked, candidate{
			key:   string(entry.Key),
			size:  uint64(entry.Size),
			score: active.score(entry, now),
		})
	}

	// Lowest score first: least recently used, least frequently used, or oldest.
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].score < ranked[j].score })

	victims := make([]string, 0, len(ranked))
	var freed uint64
	for _, cand := range ranked {
		victims = append(victims, cand.key)
		freed += cand.size
		if freed >= needed {
			break
		}
	}

	return victims
}

// scoreLRU ranks by last access: the coldest read goes first.
func scoreLRU(entry *storage.Entry, _ int64) int64 {
	return entry.LastAccess.Load()
}

// scoreFIFO ranks by insertion: the oldest entry goes first, however hot.
func scoreFIFO(entry *storage.Entry, _ int64) int64 {
	return entry.CreatedAt
}

// scoreLFU ranks by frequency, discounted by idle time.
//
// The counter is halved once per half-life since the entry was last touched,
// which is what stops a key that was hot long ago from outranking one that is
// hot now. The discount is computed here, from a field the entry already
// carries, so decay costs one shift per sampled entry and nothing walks the
// keyspace.
func (c *controller) scoreLFU(entry *storage.Entry, now int64) int64 {
	count := int64(entry.AccessCnt.Load())

	idle := now - entry.LastAccess.Load()
	if idle <= 0 {
		return count
	}

	halvings := idle / c.halfLife
	if halvings > maxHalvings {
		return 0
	}

	return count >> uint(halvings) //nolint:gosec // bounded by maxHalvings just above, and non-negative since idle > 0
}
