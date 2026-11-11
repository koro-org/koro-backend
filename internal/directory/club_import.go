package directory

import (
	"context"
	"errors"
	"strings"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/orgs"
	"koro/internal/platform/problem"
	"koro/internal/platform/textx"
)

// ImportedClub is a club found on a university's official club list
// (ingest, BE-210): only the org's facts, never a person.
type ImportedClub struct {
	UniversityID uuid.UUID
	Name         string
	// Category is never a special category (Category.Sensitive): ingest maps
	// those to "other" and lets a platform admin review the club (03 k7).
	Category Category
	// WebsiteURL is the club's own site, https only ("" none).
	WebsiteURL string
	// SocialLinks are profile links (network → https URL), never fetched.
	SocialLinks map[string]string
	// Job names the system job that imports (audit actor).
	Job string
}

// ClubImports is the system import of the clubs of an official list
// (BE-210). It joins the caller's transaction (ingest writes its own link
// row in the same one).
type ClubImports interface {
	// ImportClub returns the club of the university whose name has the
	// same ClubNameKey (created=false: an admin's, an earlier import's or a
	// claimed club, whatever its status; nothing of it changes), else opens
	// an unclaimed, non-sensitive club with an approval join policy
	// (created=true): its org (orgs.OrgAdmin.Create), its clubs row, the
	// audit record of the system job and club.updated, in the caller's
	// transaction. The university is locked for the transaction, so two
	// imports of one list never open the same club twice. 422 for an
	// inactive or unknown university, a name out of 2–200 characters, a
	// special or unknown category or a malformed link.
	ImportClub(ctx context.Context, in ImportedClub) (orgID uuid.UUID, created bool, err error)
}

var _ ClubImports = (*service)(nil)

// clubKeyNoise are the words a club's name carries on one list and not on
// another ("Bilim Kulübü" and "BİLİM TOPLULUĞU" are the same club).
var clubKeyNoise = map[string]bool{
	"kulubu": true, "kulup": true, "kulupler": true, "toplulugu": true, "topluluk": true, "club": true,
	"society": true, "community": true, "ogrenci": true, "student": true,
}

// ClubNameKey is the matching key of a club name: the parenthesised
// abbreviation dropped, Turkish-aware ASCII folding (textx.Slug), the words
// "kulübü", "topluluğu", "club" … left out. "Bilişim Kulübü (COMPEC)" and
// "BİLİŞİM TOPLULUĞU" both give "bilisim". A name of noise words only
// keeps them.
func ClubNameKey(name string) string {
	if i := strings.IndexByte(name, '('); i > 0 {
		name = name[:i]
	}
	words := strings.Split(textx.Slug(name, 0), "-")
	kept := make([]string, 0, len(words))
	for _, w := range words {
		if w != "" && !clubKeyNoise[w] {
			kept = append(kept, w)
		}
	}
	if len(kept) == 0 {
		return strings.Join(words, " ")
	}
	return strings.Join(kept, " ")
}

// ImportClub implements ClubImports.
func (s *service) ImportClub(ctx context.Context, in ImportedClub) (uuid.UUID, bool, error) {
	name, err := cleanText("name", in.Name, 2, maxName)
	if err != nil {
		return uuid.Nil(), false, err
	}
	switch {
	case !in.Category.Valid():
		return uuid.Nil(), false, problem.Field("category", "unknown category")
	case in.Category.Sensitive():
		return uuid.Nil(), false, ErrSensitivityRequired // never automatic (03 k7)
	}
	links, err := cleanSocialLinks("social_links", in.SocialLinks)
	if err != nil {
		return uuid.Nil(), false, err
	}
	key := ClubNameKey(name)
	var id uuid.UUID
	created := false
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		u, err := s.d.Store.LockUniversity(ctx, in.UniversityID)
		if errors.Is(err, ErrNotFound) || err == nil && !u.IsActive {
			return problem.Field("university_id", "must be an active university")
		}
		if err != nil {
			return err
		}
		ids, err := s.d.Store.ClubOrgsOf(ctx, u.ID)
		if err != nil {
			return err
		}
		names, err := s.d.Orgs.GetMany(ctx, ids)
		if err != nil {
			return err
		}
		for _, oid := range ids {
			if o, ok := names[oid]; ok && ClubNameKey(o.Name) == key {
				id = oid
				return nil
			}
		}
		orgID, err := s.d.Orgs.Create(ctx, orgs.CreateOrg{
			Kind: orgs.KindClub, Name: name, City: u.City, WebsiteURL: in.WebsiteURL,
		})
		if err != nil {
			return err
		}
		if _, err := s.d.Store.InsertClub(ctx, Club{
			OrgID: orgID, UniversityID: u.ID, Category: in.Category, JoinPolicy: JoinApproval,
			ClaimStatus: ClaimUnclaimed, SocialLinks: links,
		}); err != nil {
			return err
		}
		if err := s.emitClubUpdated(ctx, orgID, []string{"created"}); err != nil {
			return err
		}
		id, created = orgID, true
		return s.d.Audit.Log(ctx, auditEvent(authz.SystemActor(in.Job), authz.DirectoryClubCreate, authz.TypeClub, orgID,
			&orgID, "", map[string]any{"operation": "ingest_import", "university_id": u.ID.String(),
				"category": string(in.Category), audit.MetaSensitive: false}))
	})
	if err != nil {
		return uuid.Nil(), false, err
	}
	return id, created, nil
}
