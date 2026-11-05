package authz

// TypeMyRecommendations is the caller's own recommendations (BE-174:
// recommend.recommendation.list_own): no org, OwnerUserID = the caller, no
// Attrs. Declared by the recommend policy.
const TypeMyRecommendations = "my_recommendations"

// recommendPolicy is the policy of the recommend module (03 §5.24; BE-174):
// the rule-based event and club recommendations of a student, read from the
// student's own data for the student only. Only tickets of the recommend
// module edit this file (BE-001 R5).
//
//	recommend.recommendation.list_own  O for verified students (stu, min; the club
//	                                   and staff personas are verified students
//	                                   too); usr k-uv: 403 (no university, no
//	                                   student profile, and the recommendation
//	                                   choice lives in the student's privacy
//	                                   settings); every other role 403. Another
//	                                   user's recommendations are 404.
//
// No MFA (student roles do not require it) and no platform path: nobody
// reads another student's recommendations, support and the DPO included
// (the KVKK export carries the choice; the recommendations are computed on
// the fly and never stored).
func recommendPolicy() Policy {
	return Policy{
		Types: map[string][]string{TypeMyRecommendations: nil},
		Visible: func(a Actor, act Action, r Resource) bool {
			return act == RecommendRecommendationListOwn && r.Type == TypeMyRecommendations && a.Owns(r)
		},
		Grants: func(_ Actor, act Action, _ Resource) []Grant {
			if act == RecommendRecommendationListOwn {
				return []Grant{{Role: RoleStudent}, {Role: RoleStudentMinor}}
			}
			return nil
		},
	}
}
