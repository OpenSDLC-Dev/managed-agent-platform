package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"
)

// exitUnavailable separates "I could not ask GitHub" from "the registry has
// rotted", which log.Fatal's exit 1 reports. A scheduled run's summary is read
// by someone who was not watching, and telling them the registry rotted when
// GitHub was merely unreachable is the failure this whole tool argues against.
const exitUnavailable = 2

const usage = `usage:
  registrycheck [-file docs/DIVERGENCES.md] [-issues] [-repo owner/name] [-api URL]

Offline by default — the shape rungs alone, which is what the package's own test
runs inside the merge gate. -issues adds the rungs that ask GitHub whether each
cited issue is still open; those need the network, so they live outside
` + "`make verify`" + ` and run on a schedule instead (.github/workflows/registry.yml).`

func main() {
	log.SetFlags(0)
	file := flag.String("file", File, "the registry to check")
	issues := flag.Bool("issues", false, "also ask GitHub whether each live tracker is open")
	repo := flag.String("repo", DefaultRepo, "owner/name of the repository the pointers cite")
	api := flag.String("api", "https://api.github.com", "GitHub API root")
	flag.Usage = func() { fmt.Fprintln(os.Stderr, usage) }
	flag.Parse()

	src, err := os.ReadFile(*file)
	if err != nil {
		log.Fatal(err)
	}
	var state func(int) (bool, bool)
	if *issues {
		// Bounded, so the run cannot outlive the workflow's own timeout and
		// die without saying why.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if state, err = fetchStates(ctx, *api, *repo, Referenced(string(src))); err != nil {
			// exitUnavailable, not 1: "I could not ask GitHub" and "the
			// registry has rotted" are different facts, and a summary that
			// cannot tell them apart reports an outage as rot.
			fmt.Fprintf(os.Stderr, "cannot determine issue state: %v\n", err)
			os.Exit(exitUnavailable)
		}
	}
	findings := Check(string(src), state)
	for _, f := range findings {
		fmt.Printf("%s:%s\n", *file, f)
	}
	if len(findings) > 0 {
		// The count is the last line so a scheduled run's log tail says how
		// much rotted, not merely that something did.
		log.Fatalf("%s: %d finding(s)", *file, len(findings))
	}
	scope := "shape"
	if *issues {
		scope = "shape and issue state"
	}
	fmt.Printf("%s: clean (%s)\n", *file, scope)
}
