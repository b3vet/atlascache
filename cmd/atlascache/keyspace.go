package main

import (
	"errors"
	"time"

	"github.com/b3vet/atlascache/internal/server"
	"github.com/b3vet/atlascache/internal/storage"
)

// keyspace joins the storage engine to the seam the server declares.
//
// It exists so the dependency keeps running one way. internal/server names the
// keyspace it needs and the two failures it has a RESP reply for; internal/
// storage knows nothing of RESP. This adapter is the single place that speaks
// both vocabularies, and it lives in the composition root because that is the
// only place already entitled to know about both.
type keyspace struct {
	engine *storage.ShardedEngine
}

// The server's contract is satisfied here rather than by the engine itself, so
// a change to either side fails at the build rather than at the first GET.
var _ server.Store = keyspace{}

// Get returns the engine's read-only view of the value. The server encodes it
// into the reply before the command completes and never retains it, which is
// the contract StorageEngine documents (ADR-0013).
func (k keyspace) Get(key []byte) (value []byte, exists bool) {
	value, _, exists = k.engine.Get(key)
	return value, exists
}

// Set stores a copy of key and value, so the connection's read buffer is free
// to be reused the moment this returns (ISSUE-0009).
func (k keyspace) Set(key, value []byte, ttl time.Duration) error {
	return translate(k.engine.Set(key, value, ttl))
}

// Delete removes a key, reporting whether it removed one.
func (k keyspace) Delete(key []byte) bool {
	return k.engine.Delete(key)
}

// GetTTL reports the remaining life of a live key.
func (k keyspace) GetTTL(key []byte) (time.Duration, bool) {
	return k.engine.GetTTL(key)
}

// SetTTL attaches an expiry to a live key.
func (k keyspace) SetTTL(key []byte, ttl time.Duration) bool {
	return k.engine.SetTTL(key, ttl)
}

// translate maps the engine's sentinels onto the server's. Anything unmapped is
// passed through: the server reports an unrecognized failure as a generic RESP
// error rather than swallowing it.
func translate(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, storage.ErrValueTooLarge):
		return server.ErrValueTooLarge
	case errors.Is(err, storage.ErrOutOfMemory):
		return server.ErrOutOfMemory
	case errors.Is(err, storage.ErrInvalidKey):
		return server.ErrInvalidKey
	default:
		return err
	}
}
