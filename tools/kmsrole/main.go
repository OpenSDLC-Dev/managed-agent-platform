package main

import (
	"fmt"
	"log"
	"path/filepath"
	"strings"
)

// This binary IS the guard against the real tree: `make gcp-kms-role-check`
// runs it, and CI runs that. It prints the table it derived — which identity
// calls what, and what each is granted — before saying ok or naming what is
// under-granted, because someone about to change a role string wants to see the
// derivation and not just the verdict. Run it from the repository root.
//
// The package's own test is the other half, and stays inside `make verify`: it
// reads fixtures and the Go tree, never deploy/gcp, so the Go gate does not
// depend on the GCP tree (plan 20, Decision 9). kmsrole.go's package comment
// argues the division.
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
