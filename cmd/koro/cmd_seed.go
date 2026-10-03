package main

import (
	"context"
	"fmt"
)

// runSeed runs `koro seed ...`: synthetic data for dev/staging only, never
// production data (05 §13.7). Filled by BE-014 (bootstrap-admin), BE-020
// (reference data) and BE-033 (demo data).
func runSeed(_ context.Context, _ []string) error {
	return fmt.Errorf("%w (BE-014/BE-020/BE-033)", errNotImplemented)
}
