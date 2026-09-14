package netbench

import (
	"fmt"
	"math/rand"
)

// Workloads are pre-encoded into a table before the clock starts.
//
// Two reasons, both about not measuring the generator. Encoding a request costs
// an allocation and a few hundred nanoseconds, which is the same order as the
// round trip being measured; and a table walked in order would give each
// connection perfect key locality, which P1 already showed is worth 2.4x on the
// read path. The table is therefore built once, shuffled with a fixed seed, and
// shared read-only by every connection.

const (
	keyFormat   = "bench:key:%09d"
	missFormat  = "bench:absent:%09d"
	shuffleSeed = 0x415443 // "ATC"
)

// keyspaceFor is how many distinct keys a value size gets.
//
// It is capped by memory, not by taste: 64KB values at 100,000 keys would be
// 6.5GB on a 32GB machine, and a benchmark that swaps is measuring the page
// cache. The 64KB row is therefore a small-keyspace measurement, exactly as
// P1's 64KB row was, and must be read that way.
func keyspaceFor(valueSize int) int {
	switch {
	case valueSize >= 64*1024:
		return 4_000
	default:
		return 100_000
	}
}

func keyName(i int) []byte  { return fmt.Appendf(nil, keyFormat, i) }
func missName(i int) []byte { return fmt.Appendf(nil, missFormat, i) }

// payload is a deterministic non-repeating value of the requested size.
func payload(size int) []byte {
	buf := make([]byte, size)
	rng := rand.New(rand.NewSource(shuffleSeed))
	for i := range buf {
		buf[i] = byte('a' + rng.Intn(26))
	}
	return buf
}

func shuffle(table [][]byte) [][]byte {
	rng := rand.New(rand.NewSource(shuffleSeed))
	rng.Shuffle(len(table), func(i, j int) { table[i], table[j] = table[j], table[i] })
	return table
}

// setTable builds one SET per key across the whole keyspace.
func setTable(keyspace, valueSize int) [][]byte {
	value := payload(valueSize)
	set := []byte("SET")
	table := make([][]byte, keyspace)
	for i := range table {
		table[i] = encodeCmd(set, keyName(i), value)
	}
	return shuffle(table)
}

// getTable builds one GET per key across the whole keyspace, so a run touches
// every key rather than a hot corner of it.
func getTable(keyspace int) [][]byte {
	get := []byte("GET")
	table := make([][]byte, keyspace)
	for i := range table {
		table[i] = encodeCmd(get, keyName(i))
	}
	return shuffle(table)
}

// getMissTable reads keys that were never written. It is the floor: a GET that
// finds nothing does the least work the engine can do for a read, so the gap
// between it and a hit is what the value copy and the bulk reply cost.
func getMissTable(n int) [][]byte {
	get := []byte("GET")
	table := make([][]byte, n)
	for i := range table {
		table[i] = encodeCmd(get, missName(i))
	}
	return shuffle(table)
}

func delTable(n int) [][]byte {
	del := []byte("DEL")
	table := make([][]byte, n)
	for i := range table {
		table[i] = encodeCmd(del, keyName(i))
	}
	return shuffle(table)
}

func pingTable(n int) [][]byte {
	table := make([][]byte, n)
	req := encodeStrs("PING")
	for i := range table {
		table[i] = req
	}
	return table
}

// mixedTable is nine reads to one write, the ratio P1's BenchmarkMixed used, so
// the two are comparable.
func mixedTable(keyspace, valueSize int) [][]byte {
	value := payload(valueSize)
	get, set := []byte("GET"), []byte("SET")
	table := make([][]byte, 0, keyspace)
	for i := 0; i < keyspace; i++ {
		if i%10 == 9 {
			table = append(table, encodeCmd(set, keyName(i), value))
			continue
		}
		table = append(table, encodeCmd(get, keyName(i)))
	}
	return shuffle(table)
}
