package directory_test

import (
	"slices"
	"testing"

	"koro/db/reference"
	"koro/internal/directory"
)

// TestPlaceholderCodes (06 H-50, T-204): the embedded sample has only
// ORNEK-* university codes; a list with real codes has none.
func TestPlaceholderCodes(t *testing.T) {
	t.Parallel()
	data, err := directory.ParseReference(reference.FS)
	if err != nil {
		t.Fatal(err)
	}
	got := data.PlaceholderCodes()
	if len(got) == 0 || !slices.Contains(got, "ORNEK-ODTU") {
		t.Fatalf("PlaceholderCodes() = %v, want the ORNEK-* sample codes", got)
	}
	real := directory.ReferenceData{
		Universities: []directory.RefUniversity{{YokCode: "100170", Name: "Örnek Devlet Üniversitesi", City: "Ankara"}},
		Domains:      []directory.RefDomain{{Domain: "ornek.edu.tr", UniversityYokCode: "100170"}},
		Departments:  []directory.RefDepartment{{UniversityYokCode: "100170", Name: "Bilgisayar Mühendisliği", YokProgramCode: "100170-1"}},
	}
	if got := real.PlaceholderCodes(); len(got) != 0 {
		t.Fatalf("real codes reported as placeholders: %v", got)
	}
	real.Faculties = []directory.RefFaculty{{UniversityYokCode: "100170", Name: "Fen", YokCode: "ornek-fak"}}
	if got := real.PlaceholderCodes(); !slices.Equal(got, []string{"ornek-fak"}) {
		t.Fatalf("a lower-case placeholder code is missed: %v", got)
	}
}
