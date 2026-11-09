package sandbox

import "fmt"

// HostReadLimitation explains why FilesystemIsolation is false on the
// managed profile shipped with trpc-agent-go v1.12.0.
//
// WorkspaceWriteProfile on Linux starts bubblewrap with --ro-bind / /.
// The workspace write grant is an extra --bind. Files outside that grant
// stay readable. Writes outside it are denied. This SDK does not report
// that profile as filesystem isolation.
const HostReadLimitation = "trpc-agent-go v1.12.0 WorkspaceWriteProfile mounts the host root read-only (--ro-bind / / on Linux); files outside the workspace remain readable, so FilesystemIsolation is false. Writes outside an explicit write grant are denied."

// WindowsUnavailable is the reason reported when the host has no managed backend.
const WindowsUnavailable = "managed OS sandbox backend is not implemented on windows"

// Probe is the result of a runtime preflight. The sandbox package turns it
// into Capabilities without importing a framework.
type Probe struct {
	Platform           string
	PreflightErr       error
	NetworkRestricted  bool
	ManagedImplemented bool
}

// Assess converts a preflight into public capabilities.
//
// A Windows probe, or any probe whose ManagedImplemented field is false,
// reports every isolation flag false and sets Reason. A successful Linux or
// macOS preflight sets ManagedIsolation. FilesystemIsolation stays false
// because of HostReadLimitation. NetworkIsolation follows NetworkRestricted.
func Assess(p Probe) Capabilities {
	caps := Capabilities{Platform: p.Platform, PTY: false}
	if p.Platform == "windows" || !p.ManagedImplemented {
		if p.Platform == "windows" {
			caps.Reason = WindowsUnavailable
		} else if p.PreflightErr != nil {
			caps.Reason = p.PreflightErr.Error()
		} else {
			caps.Reason = fmt.Sprintf("managed sandbox is not implemented on %s", p.Platform)
		}
		return caps
	}
	if p.PreflightErr != nil {
		caps.Reason = p.PreflightErr.Error()
		return caps
	}
	caps.ManagedIsolation = true
	caps.FilesystemIsolation = false
	caps.NetworkIsolation = p.NetworkRestricted
	caps.Interactive = true
	caps.Reason = HostReadLimitation
	return caps
}
