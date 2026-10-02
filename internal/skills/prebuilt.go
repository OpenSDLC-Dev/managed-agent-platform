package skills

import "slices"

// PrebuiltIDs are the short ids of the reference's own prebuilt skills: the
// catalog its skills list was recorded serving (2026-09-02 batch2 #415
// `skills.list` — xlsx, pptx, pdf and docx, each `source.type: "anthropic"`),
// and the directories the operator import provisions by default
// (cmd/controlplane -import-skills).
var PrebuiltIDs = []string{"docx", "pdf", "pptx", "xlsx"}

// IsPrebuilt reports whether id is one of PrebuiltIDs.
func IsPrebuilt(id string) bool { return slices.Contains(PrebuiltIDs, id) }
