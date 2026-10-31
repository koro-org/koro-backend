package feed

// Store is the persistence boundary of feed (05 §2.2) over posts,
// post_media and feed_org_settings (02 §12.1–12.3) and the reader tables
// (post_reactions, post_saves, post_hides, org_follows, org_mutes; 02
// §12.4–12.8, BE-029). The GORM store (store_gorm.go) and the
// in-memory store (store_mem.go) pass the same contract test
// (store_contract_test.go).
//
// Every method works in the transaction carried by ctx, if any; the service
// draws the boundaries.
//
// Org scope: a post is addressed by its id (GET /posts/{post_id}); the
// service reads it with GetPost/LockPost, takes org_id from the row and
// passes that org to every later call (06 H-38). post_media has no org_id:
// ReplaceMedia writes it only through a posts row of the given org.
//
// Store is split by aggregate into embedded interfaces (05 §2.2, BE-143);
// a service file reaches the store only through the narrow view of the
// aggregate it works on (the accessors below), the retention sweeper
// through the views it holds (retention.go), the KVKK provider
// (privacy.go), which covers every table, through the whole Store.
type Store interface {
	RetentionStore // the purges of the retention sweeper (retention.go)

	postStore        // posts and their lists (store_posts.go)
	mediaStore       // post_media (store_media.go)
	settingsStore    // feed_org_settings (store_settings.go)
	interactionStore // likes, saves, hides, follows and mutes (store_interactions.go)
	reportStore      // content_reports (store_reports.go)
	moderationStore  // moderation_actions and the review queue (store_moderation.go)
	statsStore       // the counts of the panels (stats.go, BE-163)
}

// stats is the panel count view of the store (BE-163).
func (s *service) stats() statsStore { return s.d.Store }

// posts is the posts view of the store.
func (s *service) posts() postStore { return s.d.Store }

// media is the post_media view of the store.
func (s *service) media() mediaStore { return s.d.Store }

// settings is the org settings (trust) view of the store.
func (s *service) settings() settingsStore { return s.d.Store }

// interactions is the reader interactions view of the store.
func (s *service) interactions() interactionStore { return s.d.Store }

// reports is the content reports view of the store.
func (s *service) reports() reportStore { return s.d.Store }

// moderation is the moderation actions and review queue view of the store.
func (s *service) moderation() moderationStore { return s.d.Store }
