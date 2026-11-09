// Package trpcruntime assembles grape-core configuration onto tRPC-Agent-Go.
//
// Import this package as trpcruntime. Public domain types stay in the agent
// package and do not expose tRPC types. Callers that only parse skills,
// plugins, or memory files do not need to construct a Runtime.
package trpcruntime
