package sandbox

// ExtractionForTest is an Extraction at dir carrying an empty entry at each of
// paths, built without split — so a test can hand Archive what split never
// would. Test binary only.
func ExtractionForTest(dir string, tree bool, paths ...string) Extraction {
	x := Extraction{Dir: dir, tree: tree}
	for _, path := range paths {
		x.entries = append(x.entries, bulkEntry{path, nil, 0o644})
	}
	return x
}
