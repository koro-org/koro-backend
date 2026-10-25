// Package studentstest is a module's test-helper package: it may import its
// module (no violation), unlike any other package of a feature module.
package studentstest

import "koro/internal/students"

var _ = students.X
