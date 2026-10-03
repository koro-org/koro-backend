// Package ibantest holds the documented TR IBAN test vectors shared by the
// Go validator (internal/platform/iban) and the SQL function
// koro_iban_tr_valid (migration 000002; BE-002 TestKoroIbanTrValid_Vectors).
// Both must return Valid for every vector (T-42).
//
// All values are synthetic or well-known documentation examples (05 §13.7):
// bank codes 99999 and 00000 do not exist; TR33 0006 1005 1978 6457 8413 26
// is the public example IBAN used in 04.
package ibantest

// Vector is one test case. IBAN is passed as is (not normalized) to both
// iban.Valid and koro_iban_tr_valid.
type Vector struct {
	IBAN  string
	Valid bool
	Note  string
}

// Vectors returns a fresh copy of the shared vectors.
func Vectors() []Vector {
	return []Vector{
		// valid
		{"TR330006100519786457841326", true, "public documentation example (04 collection account)"},
		{"TR950001200945200058000003", true, "04 payout example with corrected check digits"},
		{"TR809999900000000000000001", true, "synthetic, bank code 99999"},
		{"TR470000000000000000000000", true, "synthetic, all-zero BBAN"},
		{"TR039999901234567890123456", true, "synthetic, check digits 03"},
		{"TR799999950000000000000000", true, "reserved 10th character is 5: not enforced (T-42)"},
		{"TR729999970000000000000099", true, "reserved 10th character is 7: not enforced (T-42)"},
		{"TR999999900000000000000047", true, "check digits 99 pass mod-97 (TR02… is canonical): ISO 02-98 range not enforced (T-101)"},
		// invalid
		{"TR760001200945200058000003", false, "04 payout example as printed: wrong check digits (T-100)"},
		{"TR330006100519786457841327", false, "last digit changed"},
		{"TR330006100519786457841362", false, "last two digits transposed"},
		{"TR340006100519786457841326", false, "check digits changed"},
		{"TR33 0006 1005 1978 6457 8413 26", false, "not normalized: spaces"},
		{"tr330006100519786457841326", false, "not normalized: lower case"},
		{"TR33000610051978645784132", false, "25 characters"},
		{"TR3300061005197864578413260", false, "27 characters"},
		{"TR33000610051978645784132A", false, "letter in BBAN"},
		{"TRAB0006100519786457841326", false, "letters as check digits"},
		{"DE89370400440532013000", false, "valid German documentation IBAN: not TR"},
		{"XX330006100519786457841326", false, "wrong country code"},
		{"TR330006100519786457841326\n", false, "trailing newline"},
		{"TR33٠٠٠6100519786457841326", false, "Arabic-Indic digits"},
		{"", false, "empty"},
	}
}
