// Package agent is the stable, framework-free protocol for a grape-core run.
//
// Types in this package are safe to serialize and to depend on from another
// product. They do not reference tRPC types. A caller starts work through a
// Runtime, consumes Events until a terminal event, and can cancel or Close
// the RunHandle. Session storage, model loops, and tool execution are provided
// by an implementation such as runtime/trpc.
package agent
