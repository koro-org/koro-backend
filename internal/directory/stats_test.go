package directory_test

import (
	"slices"
	"testing"
	"uuid"

	"koro/internal/directory"
)

// TestUniversityStructure_FacultiesAndDepartments (BE-190): the faculty
// map of the university panel holds every department of the university
// with its faculty and the faculties: the active ones and those a
// department names (an inactive faculty included), each once.
func TestUniversityStructure_FacultiesAndDepartments(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	u := e.university(t, "ORNEK-901", "Yapı Üniversitesi", "Bursa")
	fac := func(name string, active bool) directory.Faculty {
		t.Helper()
		f, err := e.mem.InsertFaculty(ctx, directory.Faculty{ID: uuid.NewV7(), UniversityID: u.ID, Name: name, IsActive: active})
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	open, closed, empty := fac("Fen Fakültesi", true), fac("Eski Fakülte", false), fac("Boş Fakülte", true)
	dept := func(name string, f *uuid.UUID) directory.Department {
		t.Helper()
		d, err := e.mem.InsertDepartment(ctx, directory.Department{ID: uuid.NewV7(), UniversityID: u.ID, FacultyID: f, Name: name,
			DegreeLevel: directory.DegreeBachelor, IsActive: true})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	phys, chem, old, loose := dept("Fizik", &open.ID), dept("Kimya", &open.ID), dept("Eski Bölüm", &closed.ID), dept("Bağımsız", nil)
	got, err := e.svc.(directory.Statistics).UniversityStructure(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantFacs := []uuid.UUID{open.ID, closed.ID, empty.ID}
	slices.SortFunc(wantFacs, uuid.UUID.Compare)
	if !slices.Equal(got.Faculties, wantFacs) {
		t.Fatalf("faculties = %v, want %v", got.Faculties, wantFacs)
	}
	of := map[uuid.UUID]*uuid.UUID{}
	for _, d := range got.Departments {
		of[d.ID] = d.FacultyID
	}
	if len(of) != 4 || *of[phys.ID] != open.ID || *of[chem.ID] != open.ID || *of[old.ID] != closed.ID || of[loose.ID] != nil {
		t.Fatalf("departments = %+v", got.Departments)
	}
	if none, err := e.svc.(directory.Statistics).UniversityStructure(ctx, uuid.NewV7()); err != nil ||
		len(none.Faculties) != 0 || len(none.Departments) != 0 {
		t.Fatalf("unknown university = %+v, %v", none, err)
	}
}
