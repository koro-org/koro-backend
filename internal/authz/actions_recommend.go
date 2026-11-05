// Action constants of the recommend module (BE-174; 03 §5.24 doc patch):
// the rule-based recommendations of uyum denetimi #25.
// TestActions_CountMatchesDesign and TestActions_MatchDesignDoc pin the
// content.

package authz

// HTTP actions of the recommend module (03 §5.24), in table order.
const (
	RecommendRecommendationListOwn Action = "recommend.recommendation.list_own"
)

// recommendActions lists the HTTP actions of recommend in 03 §5.24 order.
var recommendActions = []Action{
	RecommendRecommendationListOwn,
}
