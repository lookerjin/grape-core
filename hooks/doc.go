// Package hooks declares product-side shell hooks and runs them through an
// injected executor. It does not call os/exec. Runtime callbacks are still
// owned by the tRPC plugin or agent extension that invokes this package.
package hooks
