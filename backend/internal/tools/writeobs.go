package tools

import (
	"fmt"
	"strings"
)

// ProposedObservation is the observation a write tool hands back to the model.
//
// It has one job beyond politeness: leave the model no room to believe the write
// happened, and no reason to try again. A model that reads "sent" will tell the
// user the mail is away, and a model that reads an ambiguous acknowledgement
// will often re-issue the call — which is how one requested email becomes three
// pending approvals. So the wording is blunt about all three facts: nothing has
// happened yet, a person now decides, and repeating the call achieves nothing.
func ProposedObservation(summary string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "PROPOSED, NOT YET DONE: %s\n", summary)
	b.WriteString("Nothing has been sent, created, or changed. The request has been recorded and is " +
		"now waiting for a person to approve, edit, or reject it.\n")
	b.WriteString("This run will pause here and continue automatically once that decision is made, " +
		"and you will be told what was decided. Do not call this tool again for the same request — " +
		"the proposal already exists, and calling again would ask a person to approve the same " +
		"thing twice.\n")
	b.WriteString("If you have other things to propose or look up for this question, do them now; " +
		"otherwise stop and wait.")
	return b.String()
}
