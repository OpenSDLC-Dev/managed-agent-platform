package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// exitUnavailable separates "I could not resolve the module" from "the corpus
// has rotted", which exit 1 reports. A run whose module was missing and a run
// that found real defects must not look the same to whoever reads the summary
// afterwards.
const exitUnavailable = 2

const usage = `usage:
  sdkref [-file docs/DIVERGENCES.md] [-root .] [-comments=false] [-report] [-fail]

Reads the SDK citations in this repository and checks them against
docs/plan/51_sdk-reference-binding.md's grammar.

  (default)   rung 1 — shape, over both halves of the corpus: the document
              named by -file and the Go comments under -root
  -comments=false
              read only the document, for pointing this at one file
  -report     also run rungs 2 and 3: resolution at the pin, then the bump
              report — every anchor resolved against the pin whatever its
              stamp, plus the contradicted spans, the lag list, and everything
              not checked
  -fail       also run rung 2, and exit non-zero on any rung 1 or rung 2
              finding — or exit 2, unavailable, when the corpus cites a
              module this run could not open, or one a replace or a go.work
              points at another tree: rung 2 would skip the first in silence
              and certify the second against the wrong code. anthropic-cli is
              a checkout rather than a module, so its citations are held to
              shape alone and never make a run unavailable

make verify runs -fail over the whole corpus, through this package's own test.
Without -fail every run exits 0, which is what -report wants: the report is
read, not obeyed. ` + "`make sdk-bump-report`" + ` is the front end for -report.`

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// run is main with its plumbing passed in, so a test can assert what the flags
// promise. -fail claiming to check two rungs while checking one is the kind of
// defect only an end-to-end call can catch.
func run(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("sdkref", flag.ContinueOnError)
	fs.SetOutput(errOut)
	file := fs.String("file", File, "the document to read")
	root := fs.String("root", ".", "repository root, for resolving the pinned module")
	comments := fs.Bool("comments", true, "scan the citations in Go comments too")
	report := fs.Bool("report", false, "also run rungs 2 and 3 and print the bump report")
	fail := fs.Bool("fail", false, "exit non-zero when rungs 1 and 2 find something")
	fs.Usage = func() { fmt.Fprintln(errOut, usage) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0 // asked for, and printed
		}
		return exitUnavailable
	}

	path := *file
	if !filepath.IsAbs(path) {
		path = filepath.Join(*root, path)
	}
	findings, citations, ours, err := corpus(*root, path, *file, *comments)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return exitUnavailable
	}

	// Rung 2 belongs to -fail as much as to -report: it is half of what the
	// gate fails on, and a -fail that only checked shape would pass a corpus
	// whose anchors had all stopped resolving.
	var bump string
	var unavailable []string
	if *report || *fail {
		env, err := NewEnv(*root)
		if err != nil {
			fmt.Fprintf(errOut, "%v\n", err)
			return exitUnavailable
		}
		findings = append(findings, env.Resolution(citations)...)
		// Rung 2 can only judge a tag it can open, so it skips the citations of
		// a source this machine could not resolve, and it judges a replaced one
		// against whatever tree the replacement names. Under -fail either is
		// indistinguishable from a pass, which is the one thing this tool must
		// never let a run look like.
		unavailable = env.Unresolvable(citations)
		if *report {
			rep := env.Bump(citations)
			rep.NameOurs(ours)
			bump = rep.String()
		} else {
			// The report prints the caveats; -fail alone has no report, and a
			// verdict whose conditions went unsaid reads as unconditional.
			for _, c := range env.caveats {
				fmt.Fprintf(errOut, "caveat: %s\n", c)
			}
		}
	}

	for _, f := range findings {
		fmt.Fprintln(out, f)
	}
	fmt.Fprintf(out, "%d citation(s) in the grammar, %d finding(s)\n",
		len(citations), len(findings))
	// The report goes last. It is what a bump is read for, and printing it
	// first buries it under every line of migration debt the same run found.
	if bump != "" {
		fmt.Fprintln(out)
		fmt.Fprint(out, bump)
	}
	if *fail && len(unavailable) > 0 {
		// This outranks the findings, which are printed above either way: a run
		// that did not read part of the corpus is not a verdict on it, and exit
		// 1 would report the part it did read as the whole answer.
		for _, why := range unavailable {
			fmt.Fprintf(errOut, "cited, and rung 2 cannot judge it at its tag — %s\n", why)
		}
		return exitUnavailable
	}
	if *fail && len(findings) > 0 {
		fmt.Fprintf(errOut, "%d finding(s)\n", len(findings))
		return 1
	}
	return 0
}

// corpus reads both halves — the document named on the command line and the Go
// comments under root — and returns what rung 1 found in them together with the
// citations the resolving rungs are to judge. It is one function because the
// two halves must reach those rungs together: comment citations that rung 1
// read but rung 2 never saw would be exempt from the gate, and
// nothing about the output would say so.
//
// The third result is the coordinates into this repository both halves hold,
// which no rung checks and the report names.
func corpus(root, path, name string, comments bool) ([]Finding, []Citation, []string, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, nil, err
	}
	files, err := Tracked(root)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("indexing %s: %w", root, err)
	}
	paths, err := Requires(root)
	if err != nil {
		return nil, nil, nil, err
	}
	inRepo, requires := InSet(files), Required(paths)
	findings, ours := Scanner{InRepo: inRepo, Requires: requires}.Document(string(src), name)
	citations := Citations(string(src))
	for i := range citations {
		citations[i].File = name
	}
	if !comments {
		return findings, citations, ours, nil
	}
	cf, cc, co := GoComments(root, files, inRepo, requires)
	return append(findings, cf...), append(citations, cc...), append(ours, co...), nil
}
