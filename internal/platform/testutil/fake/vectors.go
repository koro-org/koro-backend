package fake

// IBAN test vectors live in internal/platform/iban/ibantest (BE-003): one
// source for Go iban.Valid and the SQL koro_iban_tr_valid (T-42).

// TCKNVector is a documented T.C. kimlik numarası test vector (11 digits,
// first digit non-zero, two check digits).
type TCKNVector struct {
	Input string
	Valid bool
	Note  string
}

// TCKNVectors are the only national ID numbers allowed in tests and seeds.
// The valid ones are well-known test numbers with a correct checksum.
var TCKNVectors = []TCKNVector{
	{"10000000146", true, "widely used test number"},
	{"11111111110", true, "repeated digits, valid checksum"},
	{"10000000145", false, "wrong 11th digit"},
	{"10000000136", false, "wrong 10th digit"},
	{"01234567890", false, "leading zero"},
	{"1000000014", false, "10 digits"},
	{"100000001460", false, "12 digits"},
	{"1000000014A", false, "letter"},
}
