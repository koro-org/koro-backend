// Package reference embeds the public reference data loaded by `koro seed
// reference` (BE-020; 02 §8, §23: reference data is a command, not a
// migration): universities, their .edu.tr e-mail domains, faculties,
// departments and the skill taxonomy. Only public data; no personal data.
//
// Each CSV file starts with comment lines (#) naming its source and licence.
// Changing the list (PS-09 pilot universities) means editing these files
// only: the import is an idempotent upsert, so re-running it is safe.
package reference

import "embed"

// FS holds the reference CSV files.
//
//go:embed *.csv
var FS embed.FS
