package threads

import (
	"github.com/ahsanulks/social-scheduler/internal/scheduling/adapters/publishers/graphapi"
)

// classify maps Graph API failures onto the adapter's retry/permanent split.
// The rules are shared across Meta's Graph APIs; see the graphapi package.
func classify(status int, body []byte) error {
	return graphapi.Classify("threads", status, body)
}
