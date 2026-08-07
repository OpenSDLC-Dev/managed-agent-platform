package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"
)

const usage = `usage:
  changelog assemble -version X.Y.Z [-date YYYY-MM-DD] [-changelog CHANGELOG.md] [-dir changelog.d]
  changelog notes    -version X.Y.Z [-out FILE] [-cap BYTES] [-changelog CHANGELOG.md]
  changelog latest   [-changelog CHANGELOG.md]
  changelog archive  -version X.Y.Z [-changelog CHANGELOG.md] [-dir docs/changelog]`

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		log.Fatal(usage)
	}
	switch os.Args[1] {
	case "assemble":
		fs := flag.NewFlagSet("assemble", flag.ExitOnError)
		version := fs.String("version", "", "release version (X.Y.Z, required)")
		date := fs.String("date", time.Now().UTC().Format("2006-01-02"), "release date (YYYY-MM-DD)")
		changelog := fs.String("changelog", "CHANGELOG.md", "path to the changelog")
		dir := fs.String("dir", "changelog.d", "fragment directory")
		_ = fs.Parse(os.Args[2:])
		if err := runAssemble(*changelog, *dir, *version, *date); err != nil {
			log.Fatal(err)
		}
	case "notes":
		fs := flag.NewFlagSet("notes", flag.ExitOnError)
		version := fs.String("version", "", "release version (X.Y.Z, required)")
		out := fs.String("out", "", "output file (default stdout)")
		cap := fs.Int("cap", 0, "clamp the body to this many bytes (0 = unlimited)")
		changelog := fs.String("changelog", "CHANGELOG.md", "path to the changelog")
		_ = fs.Parse(os.Args[2:])
		if err := runNotes(*changelog, *version, *out, *cap); err != nil {
			log.Fatal(err)
		}
	case "latest":
		fs := flag.NewFlagSet("latest", flag.ExitOnError)
		changelog := fs.String("changelog", "CHANGELOG.md", "path to the changelog")
		_ = fs.Parse(os.Args[2:])
		content, err := os.ReadFile(*changelog)
		if err != nil {
			log.Fatal(err)
		}
		v, err := latest(string(content))
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(v)
	case "archive":
		fs := flag.NewFlagSet("archive", flag.ExitOnError)
		version := fs.String("version", "", "released version to archive (X.Y.Z, required)")
		changelog := fs.String("changelog", "CHANGELOG.md", "path to the changelog")
		dir := fs.String("dir", "docs/changelog", "archive directory")
		_ = fs.Parse(os.Args[2:])
		if err := runArchive(*changelog, *dir, *version); err != nil {
			log.Fatal(err)
		}
	default:
		log.Fatal(usage)
	}
}
