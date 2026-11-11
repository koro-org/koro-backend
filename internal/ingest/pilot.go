package ingest

import (
	"context"
	"slices"
	"strconv"
	"uuid"

	"koro/internal/authz"
)

// The built-in sources of the pilot universities (research §2, looked at
// on 2026-11-11; robots.txt allows every URL below). The admin adds them
// for a university of the catalog with adminAddPilotIngestSources: the
// university IDs are the database's, not the code's.

// PilotInput is the body of adminAddPilotIngestSources.
type PilotInput struct {
	Pilot        string // ytu | boun | sabanci | metu | itu
	UniversityID uuid.UUID
}

// PilotResult tells what adminAddPilotIngestSources did.
type PilotResult struct {
	Created  int
	Existing int
	Sources  []Source
}

type pilotSource struct {
	kind     Kind
	adapter  AdapterName
	url      string
	label    string
	disabled bool // starts disabled whatever the environment (known 403)
}

// sabanciCategories are the values of studentlife.sabanciuniv.edu's
// category filter (one list source per category: the label comes with it).
var sabanciCategories = []int{5, 3, 2, 6, 1, 4}

var pilots = func() map[string][]pilotSource {
	su := make([]pilotSource, 0, len(sabanciCategories))
	for _, c := range sabanciCategories {
		su = append(su, pilotSource{kind: KindClubList, adapter: AdapterClubListSU,
			url:   "https://studentlife.sabanciuniv.edu/kulupler?c=" + strconv.Itoa(c),
			label: "Sabancı kulüp listesi (kategori " + strconv.Itoa(c) + ")"})
	}
	return map[string][]pilotSource{
		"ytu": {
			{kind: KindClubList, adapter: AdapterClubListYTU, label: "YTÜ resmî öğrenci kulüpleri listesi",
				url: "https://www.yildiz.edu.tr/kampus/kampus-yasami/ogrenci-kulupleri"},
		},
		"boun": {
			{kind: KindClubList, adapter: AdapterClubListBOUN, label: "Boğaziçi kulüpler ve topluluklar",
				url: "https://kultursanat.bogazici.edu.tr/tr/pages/kulupler-ve-topluluklar/2482"},
			{kind: KindEvents, adapter: AdapterSitemap, label: "Boğaziçi öğrenci takvimi",
				url: "https://takvim.bogazici.edu.tr/tr/site/events"},
		},
		"sabanci": su,
		"metu": {
			{kind: KindClubList, adapter: AdapterClubListMETU, label: "ODTÜ öğrenci toplulukları",
				url: "https://kim.metu.edu.tr/tr/ogrenci-topluluklari"},
			{kind: KindEvents, adapter: AdapterHTMLPage, label: "ODTÜ KİM etkinlikler",
				url: "https://kim.metu.edu.tr/tr/etkinlikler"},
		},
		"itu": {
			{kind: KindClubList, adapter: AdapterClubListITU, label: "İTÜ kulüpler (403, doğrulanamadı)",
				url: "https://kulupler.itu.edu.tr/", disabled: true},
			{kind: KindEvents, adapter: AdapterJSONAPI, label: "İTÜ etkinlik takvimi (JSON)",
				url: "https://etkinlik.itu.edu.tr/public/embed/etkinlikler.aspx"},
		},
	}
}()

// PilotKeys lists the pilot keys (OpenAPI enum).
func PilotKeys() []string { return []string{"ytu", "boun", "sabanci", "metu", "itu"} }

// AdminAddPilotSources implements Service: the pilot's sources for the
// university, idempotent (a URL that exists is counted, not changed).
func (s *service) AdminAddPilotSources(ctx context.Context, a authz.Actor, in PilotInput) (PilotResult, error) {
	if err := authz.Can(ctx, a, authz.IngestSourceCreate, adminSurface); err != nil {
		return PilotResult{}, err
	}
	set, ok := pilots[in.Pilot]
	if !ok || !slices.Contains(PilotKeys(), in.Pilot) {
		return PilotResult{}, ErrUnknownPilot
	}
	rows := make([]Source, 0, len(set))
	for _, p := range set {
		in := SourceInput{Kind: p.kind, Adapter: p.adapter, URL: p.url, Label: p.label, UniversityID: in.UniversityID}
		if p.disabled {
			in.Enabled = ptr(false)
		}
		src, err := s.checkSource(ctx, in)
		if err != nil {
			return PilotResult{}, err
		}
		rows = append(rows, src)
	}
	var res PilotResult
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		res = PilotResult{}
		for _, src := range rows {
			row, created, err := s.d.Store.InsertSource(ctx, src)
			if err != nil {
				return err
			}
			res.Sources = append(res.Sources, row)
			if !created {
				res.Existing++
				continue
			}
			res.Created++
			if err := s.auditSource(ctx, a, authz.IngestSourceCreate, row, map[string]any{"pilot": in.Pilot}); err != nil {
				return err
			}
		}
		return nil
	})
	return res, err
}
