// Package sandbox describes process isolation without binding callers to a
// framework.
//
// Capabilities come from a backend preflight. A false ManagedIsolation value
// is not a guess that the caller may ignore: command execution must fail
// closed. Windows has no managed native backend in this module.
package sandbox
