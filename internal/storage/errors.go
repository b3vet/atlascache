// Package storage provides the core key-value storage engine.
package storage

import "errors"

// Sentinel errors for common conditions
var (
	// ErrKeyNotFound indicates the key does not exist
	ErrKeyNotFound = errors.New("key not found")

	// ErrKeyExists indicates the key already exists (for SETNX)
	ErrKeyExists = errors.New("key already exists")

	// ErrValueTooLarge indicates the value exceeds max size
	ErrValueTooLarge = errors.New("value exceeds maximum size")

	// ErrOutOfMemory indicates max memory limit reached
	ErrOutOfMemory = errors.New("out of memory")

	// ErrEngineClosed indicates the engine has been closed
	ErrEngineClosed = errors.New("storage engine closed")

	// ErrInvalidTTL indicates an invalid TTL value
	ErrInvalidTTL = errors.New("invalid TTL value")

	// ErrInvalidKey indicates an invalid key
	ErrInvalidKey = errors.New("invalid key")

	// ErrEvictionRequired indicates eviction is needed but no policy is set
	ErrEvictionRequired = errors.New("eviction required but no eviction policy configured")
)
