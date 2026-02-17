// constellation_singleton.go - Singleton constellation DB connection
//
// Avoids 2-5ms overhead per request from repeated Open/PRAGMA/migration checks.
// The connection stays open for the process lifetime.

package main

import (
	"sync"

	"github.com/cogos-dev/cogos/sdk/constellation"
)

var (
	constellationOnce sync.Once
	constellationDB   *constellation.Constellation
	constellationErr  error
)

// getConstellation returns a shared constellation database connection.
// The connection is lazily initialized on first call and reused thereafter.
// Callers must NOT call Close() on the returned Constellation.
func getConstellation() (*constellation.Constellation, error) {
	constellationOnce.Do(func() {
		root, _, err := ResolveWorkspace()
		if err != nil {
			constellationErr = err
			return
		}
		constellationDB, constellationErr = constellation.Open(root)
	})
	if constellationErr != nil {
		return nil, constellationErr
	}
	return constellationDB, nil
}

// CloseConstellation closes the singleton constellation connection.
// Call this during graceful shutdown if needed.
func CloseConstellation() {
	if constellationDB != nil {
		constellationDB.Close()
	}
}
