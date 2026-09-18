package main

import (
	"fmt"
	"log"
	"path/filepath"
	"strings"
)

// The guard itself runs inside `make verify`, through this package's own test.
// This binary exists for the other reader of the same derivation: someone
// editing deploy/gcp/environment/iam.tf, who wants to see which identity calls
// what before changing a role string. Run it from the repository root.
func main() {
	log.SetFlags(0)
	r, err := Check(".", filepath.Join("deploy", "gcp"))
	if err != nil {
		log.Fatalf("kmsrole: %v", err)
	}
	for _, row := range r.Rows {
		role := "(no grant)"
		if row.Grant != nil {
			role = row.Grant.Role
		}
		fmt.Printf("%-13s calls %-19s granted %s\n", row.Binary, row.Needs, role)
		for _, s := range row.Sites {
			fmt.Printf("%16s%s\n", "", s)
		}
	}
	if len(r.Findings) > 0 {
		var b strings.Builder
		for _, f := range r.Findings {
			fmt.Fprintf(&b, "\n%s", f)
		}
		log.Fatalf("%d identity/identities granted less than their code calls:%s", len(r.Findings), b.String())
	}
	fmt.Println("ok")
}
