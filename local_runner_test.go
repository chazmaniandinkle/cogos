// local_runner_test.go — Coverage for the LocalRunner ownership-verification
// path and the cmd-hash hashing surface.
//
// These tests do NOT spawn real processes; they exercise the unit logic of
// verifyOwnership via the psLookup seam and localCmdHash via direct calls.
// The adoption test uses os.Getpid() so processAlive() returns true without
// fork/exec gymnastics, and feeds the expected argv through the psLookup seam.

package main

import (
	"errors"
	"os"
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

// legacyPIDFile is the on-disk shape older kernel builds wrote before we
// started recording Cmd/Args on LocalProcess. Emulating it in tests lets us
// exercise the adoption path without a migration dance.
func writeLegacyPIDFile(t *testing.T, root, name string, pid int) {
	t.Helper()
	legacy := &LocalProcess{
		Name:      name,
		PID:       pid,
		StartedAt: "2026-01-01T00:00:00Z",
		CmdHash:   "legacy-hash",
		Workdir:   root,
		LogPath:   localLogPath(root, name),
		// Cmd and Args intentionally empty — this is what older PID files
		// on disk look like, and what LocalStatusWithCRD must handle.
	}
	if err := writeLocalProcess(root, legacy); err != nil {
		t.Fatalf("write legacy PID file: %v", err)
	}
}

// TestLocalStatusWithCRD_AdoptsLegacyPIDFile covers the in-place upgrade
// recovery path: older kernel wrote a PID file without Cmd/Args, the service
// is still running, and the current kernel must adopt it — not clear the PID
// file and start a duplicate alongside the live service.
//
// The test uses os.Getpid() so processAlive() returns true without spawning,
// and routes the ps argv lookup through the psLookup seam so the assertion is
// hermetic (no dependency on what `ps -p $OS_PID -o args=` actually emits).
func TestLocalStatusWithCRD_AdoptsLegacyPIDFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("adoption path uses ps, which isn't the production path on windows")
	}

	root := t.TempDir()
	name := "myservice"
	pid := os.Getpid() // always alive and owned by this process
	writeLegacyPIDFile(t, root, name, pid)

	crd := &ServiceCRD{
		Metadata: ServiceCRDMeta{Name: name},
		Spec: ServiceCRDSpec{
			Local: &ServiceLocal{
				Command: "/usr/bin/python3",
				Args:    []string{"server.py", "--port", "9090"},
			},
		},
	}
	expectedArgv := "/usr/bin/python3 server.py --port 9090"

	withPSLookup(func(lookupPID int) (string, error) {
		if lookupPID != pid {
			t.Fatalf("ps consulted for unexpected pid %d (want %d)", lookupPID, pid)
		}
		return expectedArgv, nil
	}, func() {
		p, err := LocalStatusWithCRD(root, name, crd)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p == nil {
			t.Fatalf("expected non-nil LocalProcess after adoption")
		}
		if !p.Running {
			t.Errorf("Running = false, want true after adoption")
		}
		if p.Cmd != "/usr/bin/python3" {
			t.Errorf("returned Cmd = %q, want /usr/bin/python3 (post-adoption)", p.Cmd)
		}
		want := []string{"server.py", "--port", "9090"}
		if len(p.Args) != len(want) {
			t.Fatalf("Args len = %d, want %d (%v)", len(p.Args), len(want), p.Args)
		}
		for i := range want {
			if p.Args[i] != want[i] {
				t.Errorf("Args[%d] = %q, want %q", i, p.Args[i], want[i])
			}
		}
	})

	// Verify the PID file was rewritten with the recorded cmd+args so
	// subsequent calls take the strict fast path and no longer need the CRD.
	rewritten, err := readLocalProcess(root, name)
	if err != nil {
		t.Fatalf("read rewritten PID file: %v", err)
	}
	if rewritten == nil {
		t.Fatalf("PID file was cleared instead of rewritten")
	}
	if rewritten.Cmd != "/usr/bin/python3" {
		t.Errorf("rewritten Cmd = %q, want /usr/bin/python3", rewritten.Cmd)
	}
	if len(rewritten.Args) != 3 {
		t.Errorf("rewritten Args = %v, want 3 elements", rewritten.Args)
	}
}

// Negative case: legacy PID file + CRD whose expected argv does NOT match the
// live process. Must clear the PID file (old safe-default) — adopting a
// mismatched process would let a crashed-and-PID-reused scenario slip through.
func TestLocalStatusWithCRD_ClearsLegacyPIDOnArgvMismatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("adoption path uses ps, which isn't the production path on windows")
	}

	root := t.TempDir()
	name := "mismatched"
	pid := os.Getpid()
	writeLegacyPIDFile(t, root, name, pid)

	crd := &ServiceCRD{
		Metadata: ServiceCRDMeta{Name: name},
		Spec: ServiceCRDSpec{
			Local: &ServiceLocal{
				Command: "/usr/bin/python3",
				Args:    []string{"server.py"},
			},
		},
	}

	withPSLookup(func(lookupPID int) (string, error) {
		// Live process is some unrelated shell — definitely not our service.
		return "-zsh", nil
	}, func() {
		p, err := LocalStatusWithCRD(root, name, crd)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p == nil {
			t.Fatalf("readLocalProcess should have returned the pre-clear struct")
		}
		if p.Running {
			t.Errorf("Running = true, want false (argv mismatch must not adopt)")
		}
	})

	// PID file must be gone so the next reconcile creates a fresh process.
	if _, statErr := os.Stat(localPIDPath(root, name)); !os.IsNotExist(statErr) {
		t.Errorf("legacy PID file should have been cleared (argv mismatch); stat err=%v", statErr)
	}
}

// Legacy PID file + no CRD → preserves original strict behavior (clear).
func TestLocalStatusWithCRD_NilCRDClearsLegacyPID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("adoption path uses ps, which isn't the production path on windows")
	}

	root := t.TempDir()
	name := "nocrd"
	pid := os.Getpid()
	writeLegacyPIDFile(t, root, name, pid)

	// psLookup must NOT be consulted at all — without a CRD we can't form an
	// expected argv, so there's nothing to compare. Fail loudly if called.
	withPSLookup(func(lookupPID int) (string, error) {
		t.Fatalf("ps should not be consulted when crd is nil")
		return "", nil
	}, func() {
		p, err := LocalStatusWithCRD(root, name, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p != nil && p.Running {
			t.Errorf("Running = true, want false (no CRD must clear legacy)")
		}
	})

	if _, statErr := os.Stat(localPIDPath(root, name)); !os.IsNotExist(statErr) {
		t.Errorf("legacy PID file should have been cleared when CRD is nil; stat err=%v", statErr)
	}

	// Unused import guard: errors used implicitly via withPSLookup/psLookup stubs.
	_ = errors.New("")
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
