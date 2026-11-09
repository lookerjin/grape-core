package sandbox

import (
	"errors"
	"strings"
	"testing"
)

func TestAssessWindowsIsUnavailable(t *testing.T) {
	caps := Assess(Probe{Platform: "windows", ManagedImplemented: false})
	if caps.ManagedIsolation || caps.FilesystemIsolation || caps.NetworkIsolation || caps.PTY || caps.Interactive {
		t.Fatalf("windows caps %#v", caps)
	}
	if !strings.Contains(caps.Reason, "windows") {
		t.Fatalf("reason %q", caps.Reason)
	}
}

func TestAssessPreflightFailureKeepsReason(t *testing.T) {
	caps := Assess(Probe{
		Platform:           "linux",
		ManagedImplemented: true,
		PreflightErr:       errors.New("bubblewrap preflight failed"),
		NetworkRestricted:  true,
	})
	if caps.ManagedIsolation || caps.NetworkIsolation {
		t.Fatalf("failed preflight looked healthy: %#v", caps)
	}
	if !strings.Contains(caps.Reason, "bubblewrap") {
		t.Fatalf("reason %q", caps.Reason)
	}
}

func TestAssessSuccessDoesNotClaimHostReadIsolation(t *testing.T) {
	caps := Assess(Probe{
		Platform:           "linux",
		ManagedImplemented: true,
		NetworkRestricted:  true,
		SensitivePaths:     []string{"/home/looker/.ssh"},
	})
	if !caps.ManagedIsolation || !caps.NetworkIsolation {
		t.Fatalf("caps %#v", caps)
	}
	if caps.FilesystemIsolation {
		t.Fatal("filesystem isolation was claimed")
	}
	if len(caps.SensitivePathsMasked) != 1 || caps.SensitivePathsMasked[0] != "/home/looker/.ssh" {
		t.Fatalf("masked %#v", caps.SensitivePathsMasked)
	}
	if !strings.Contains(caps.Reason, "--ro-bind") {
		t.Fatalf("reason %q", caps.Reason)
	}
	open := Assess(Probe{Platform: "linux", ManagedImplemented: true, NetworkRestricted: false})
	if open.NetworkIsolation {
		t.Fatal("enabled network was reported as isolated")
	}
}
