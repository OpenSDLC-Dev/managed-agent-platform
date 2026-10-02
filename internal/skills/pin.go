package skills

import (
	"regexp"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// PinForm is the form of the version a stored skills[] entry pins (plan 39
// decision 5): the alias, a version id, the legacy numeric, or none of them.
type PinForm int

const (
	// PinNone addresses no version: the reference is dangling.
	PinNone PinForm = iota
	// PinLatest is the alias LatestAlias, the newest version at use time.
	PinLatest
	// PinID is a version id, in the GA skver_ spelling or the legacy
	// skillver_ one, naming its row under the skill it pins — one the
	// reference minted included, which no row here carries.
	PinID
	// PinNumber is the legacy numeric version, already concrete.
	PinNumber
)

// LatestAlias is the one alias a pin may hold.
const LatestAlias = "latest"

var pinDigitsRe = regexp.MustCompile(`^[0-9]+$`)

// ClassifyPin reads a stored pin's form, the one reading every resolver of a
// stored pin shares — the brain's Level-1 injection, the executor's
// materialization and the agent create's check of an anthropic reference —
// so that "is it digits?" can never again resolve a pinned id to the newest
// version. The id form asks for a well-formed id, not merely a prefixed one:
// nothing validates a stored pin's shape on the way in, and an unstorable
// byte must reach no bind parameter. Well-formed is this platform's alphabet
// or the reference's (domain.WellFormedID), so a version id the reference
// minted reads as the id it is, resolving to nothing here.
//
// It is not the API's {version} path slot, whose grammar is the wire's and
// is validated per route (checkSkillVersion), nor the BYOC worker's, which
// reads pins off the wire and answers by a retrieve.
func ClassifyPin(version string) PinForm {
	switch {
	case version == LatestAlias:
		return PinLatest
	case domain.ID(version).HasPrefix(domain.PrefixSkillVersion) && domain.ID(version).Valid(),
		domain.WellFormedID(version, domain.PrefixSkillVersion):
		return PinID
	case pinDigitsRe.MatchString(version):
		return PinNumber
	}
	return PinNone
}
