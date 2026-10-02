package skills

// referencePrebuilt is the reference's own prebuilt skill catalog as recorded:
// its skills list served xlsx, pptx, pdf and docx, each `source.type:
// "anthropic"` (2026-09-02 batch2 #415 `skills.list`). It is the recording's
// set, not the operator import's default list (cmd/controlplane
// -import-skills), so widening what an import provisions cannot widen what an
// agent create leaves unchecked.
var referencePrebuilt = map[string]bool{"docx": true, "pdf": true, "pptx": true, "xlsx": true}

// IsReferencePrebuilt reports whether id names one of the reference's own
// prebuilt skills.
func IsReferencePrebuilt(id string) bool { return referencePrebuilt[id] }
