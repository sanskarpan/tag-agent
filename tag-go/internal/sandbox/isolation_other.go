//go:build !darwin && !linux

package sandbox

import "time"

// buildIsolation fails closed on platforms where we have no OS-level
// confinement primitive to offer. Running the command unconfined under a flag
// literally named `restricted` would be a lie, so we refuse instead.
//
// --allow-unconfined (the third parameter) is deliberately NOT honoured here:
// on Linux it still buys rlimits and a network namespace, but on a platform with
// no primitive at all it would buy literally nothing.
func buildIsolation(_ string, _ time.Duration, _ bool) (*isolationPlan, *Result, error) {
	return nil, &Result{
		Exit: 127,
		Stderr: "sandbox: no OS-level isolation is available on this platform. " +
			"Use --backend docker for isolated execution.",
		Isolation: "none (failed closed: unsupported platform)",
	}, nil
}
