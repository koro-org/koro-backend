// Action constants of the stats module (BE-163; 03 §5.23 doc patch): the
// panel summaries of SR §8. TestActions_CountMatchesDesign and
// TestActions_MatchDesignDoc pin the content.

package authz

// HTTP actions of the stats module (03 §5.23), in table order.
const (
	StatsPlatformRead   Action = "stats.platform.read"
	StatsClubRead       Action = "stats.club.read"
	StatsCompanyRead    Action = "stats.company.read"
	StatsUniversityRead Action = "stats.university.read"
)

// statsActions lists the HTTP actions of stats in 03 §5.23 order.
var statsActions = []Action{
	StatsPlatformRead,
	StatsClubRead,
	StatsCompanyRead,
	StatsUniversityRead,
}
