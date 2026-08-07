package main

import (
	"flag"
	"log"
	"os"
	"time"
)

const usage = `usage:
  changelog assemble -version X.Y.Z [-date YYYY-MM-DD] [-changelog CHANGELOG.md] [-dir changelog.d]
  changelog notes    -version X.Y.Z [-out FILE] [-changelog CHANGELOG.md]`

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
		changelog := fs.String("changelog", "CHANGELOG.md", "path to the changelog")
		_ = fs.Parse(os.Args[2:])
		if err := runNotes(*changelog, *version, *out); err != nil {
			log.Fatal(err)
		}
	default:
		log.Fatal(usage)
	}
}
