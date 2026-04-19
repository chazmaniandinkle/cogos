// local_runner_test.go — Coverage for the LocalRunner ownership-verification
// path and the cmd-hash hashing surface.
//
// These tests do NOT spawn real processes; they exercise the unit logic of
// verifyOwnership via the psLookup seam and localCmdHash via direct calls.

package main

import (
	"errors"
	"runtime"
	"testing"
)

// withPSLookup swaps psLookup for the duration of fn, restoring it after.
// Keeps tests isolated from the real /bin/ps so they don't depend on what
// PIDs happen to exist on the test runner.
func withPSLookup(stub func(pid int) (string, error), fn func()) {
	orig := psLookup
	psLookup = stub
	defer func() { psLookup = orig }()
	fn()
}

func TestVerifyOwnership(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("verifyOwnership uses ps, which isn't the production path on windows")
	}

	cases := []struct {
		name         string
		pid          int
		expectedCmd  string
		expectedArgs []string
		psStub       func(pid int) (string, error)
		want         bool
	}{
		{
			name:         "matches cmd with no args",
			pid:          1234,
			expectedCmd:  "/usr/bin/python3",
			expectedArgs: nil,
			psStub: func(pid int) (string, error) {
				return "/usr/bin/python3", nil
			},
			want: true,
		},
		{
			name:         "matches cmd with args",
			pid:          1234,
			expectedCmd:  "/opt/venv/bin/python3",
			expectedArgs: []string{"server.py", "--port", "8080"},
			psStub: func(pid int) (string, error) {
				return "/opt/venv/bin/python3 server.py --port 8080", nil
			},
			want: true,
		},
		{
			name:         "mismatched cmd (PID reuse) → false",
			pid:          1234,
			expectedCmd:  "/opt/venv/bin/python3",
			expectedArgs: []string{"server.py"},
			psStub: func(pid int) (string, error) {
				// User's bash shell happens to have reused PID 1234.
				return "-zsh", nil
			},
			want: false,
		},
		{
			name:         "mismatched args → false",
			pid:          1234,
			expectedCmd:  "/usr/bin/python3",
			expectedArgs: []string{"server.py", "--port", "8080"},
			psStub: func(pid int) (string, error) {
				return "/usr/bin/python3 server.py --port 9090", nil
			},
			want: false,
		},
		{
			name:         "ps says no such process → false (treated as dead)",
			pid:          1234,
			expectedCmd:  "/usr/bin/python3",
			expectedArgs: []string{"server.py"},
			psStub: func(pid int) (string, error) {
				return "", errors.New("ps: no such process")
			},
			want: false,
		},
		{
			name:         "empty ps output → false",
			pid:          1234,
			expectedCmd:  "/usr/bin/python3",
			expectedArgs: nil,
			psStub: func(pid int) (string, error) {
				return "   ", nil
			},
			want: false,
		},
		{
			name:         "pid <= 0 → false without consulting ps",
			pid:          0,
			expectedCmd:  "/usr/bin/python3",
			expectedArgs: nil,
			psStub: func(pid int) (string, error) {
				t.Fatalf("ps should not be consulted for pid<=0")
				return "", nil
			},
			want: false,
		},
		{
			name:         "legacy PID file (no recorded cmd) → false",
			pid:          1234,
			expectedCmd:  "",
			expectedArgs: nil,
			psStub: func(pid int) (string, error) {
				t.Fatalf("ps should not be consulted when expectedCmd is empty")
				return "/usr/bin/anything", nil
			},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withPSLookup(tc.psStub, func() {
				got, err := verifyOwnership(tc.pid, tc.expectedCmd, tc.expectedArgs)
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got != tc.want {
					t.Fatalf("verifyOwnership(%d, %q, %v) = %v, want %v",
						tc.pid, tc.expectedCmd, tc.expectedArgs, got, tc.want)
				}
			})
		})
	}
}

func TestLocalCmdHashIncludesVenv(t *testing.T) {
	base := &ServiceLocal{
		Command: "python3",
		Args:    []string{"server.py", "--port", "8080"},
		Env:     map[string]string{"FOO": "bar"},
	}
	withVenv := *base
	withVenv.Venv = ".venv"

	hashWithoutVenv := localCmdHash(base, "/workspace")
	hashWithVenv := localCmdHash(&withVenv, "/workspace")

	if hashWithoutVenv == hashWithVenv {
		t.Fatalf("localCmdHash must differ when venv changes; got %q for both",
			hashWithoutVenv)
	}

	// Sanity: same inputs produce the same hash (stable).
	again := localCmdHash(base, "/workspace")
	if again != hashWithoutVenv {
		t.Fatalf("localCmdHash not stable across calls: %q vs %q", again, hashWithoutVenv)
	}

	// Swapping the venv to a different path also flips the hash.
	otherVenv := *base
	otherVenv.Venv = "/abs/other/.venv"
	otherHash := localCmdHash(&otherVenv, "/workspace")
	if otherHash == hashWithVenv {
		t.Fatalf("localCmdHash must differ across distinct venv paths; got %q for both",
			otherHash)
	}
}
