// Command thirdpartylicenses writes THIRD_PARTY_LICENSES at the repository
// root (`make third-party-licenses`): the license texts of what the ripgrep
// binaries internal/ripgrep embeds were statically built from, which the
// worker tarballs and the server image ship beside NOTICE.
//
// Every input is pinned in sources.json by URL and sha256, and nothing is
// taken on trust: the Cargo.lock files of ripgrep's release and of the Rust
// standard library each build was compiled with; every crates.io package
// those lock files name, fetched from static.crates.io and checked against
// the lock file's own checksum; and the upstream license files of what no
// crate carries — ripgrep's own, PCRE2's (pcre2-sys vendors PCRE2's sources
// without it), musl's, Rust's, compiler-builtins' and LLVM's compiler-rt and
// libunwind. jemalloc's comes inside the tikv-jemalloc-sys crate, which
// vendors it. Each crate contributes every file in its archive named as a
// license, copyright, notice or authors file, at any depth, so a vendored
// library's own license comes along. A package a lock file names by path —
// its own workspace, which crates.io does not serve — must be put under one
// of those upstream components in sources.json, or the run stops: not every
// workspace crate is under its project's terms (the standard library's
// compiler_builtins is not).
//
// The crate list is a superset, said so in the file it writes: which of a
// lock file's crates a build links depends on its target and features, which
// this does not resolve, so build tools and other platforms' crates are
// included too. Each text is written as its source has it but for
// whitespace no text's meaning rests on (normText): CRLF line ends are made
// LF, the spaces, tabs and carriage returns that end a line are dropped, and
// so are the blank lines that end a text. So no line ends in whitespace, and
// the file ends in no blank line, which `git diff --check` would flag; a
// space before a tab in a line's indent, which it flags as well, is left as
// the source has it. The output is a function of the pinned bytes alone, so
// running it twice writes the same file. Its header records the sha256 of
// sources.json and of the rest of the file, which is what its test can hold
// the committed file to without the network (TestTheGeneratedFileIsCurrent).
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
)

// source is one pinned input: a lock file (Name) or an upstream license file
// (Component).
type source struct {
	Name      string `json:"name,omitempty"`
	Component string `json:"component,omitempty"`
	Note      string `json:"note,omitempty"`
	URL       string `json:"url"`
	SHA256    string `json:"sha256"`
	// PathPackages, on a lock file, puts each package the lock file names by
	// path, keyed "<name> <version>", under the component in files whose
	// texts cover it.
	PathPackages map[string]string `json:"path_packages,omitempty"`
}

// sources is sources.json. CrateFiles, keyed "<name> <version>", supplies
// the license files of a crate whose archive carries none, from its
// repository at the commit the crate was published from.
type sources struct {
	Ripgrep    string              `json:"ripgrep"`
	Lockfiles  []source            `json:"lockfiles"`
	Files      []source            `json:"files"`
	CrateFiles map[string][]source `json:"crate_files"`
}

// registry is the source a Cargo.lock names crates.io packages by.
const registry = "registry+https://github.com/rust-lang/crates.io-index"

// crateURL is where crates.io serves a package's archive.
var crateURL = func(name, version string) string {
	return "https://static.crates.io/crates/" + name + "/" + name + "-" + version + ".crate"
}

// maxDownload bounds one download: the largest pinned input, a crate that
// vendors a C library, is a few MB.
const maxDownload = 64 << 20

// licenseName is a file a crate ships its license terms in.
var licenseName = regexp.MustCompile(`(?i)^(licen[cs]e|copying|copyright|unlicense|notice|authors)([-._].*)?$`)

// cargoLicense reads the license expression of a crate's Cargo.toml.
var cargoLicense = regexp.MustCompile(`(?m)^license\s*=\s*"([^"]*)"`)

// lockField reads one of a Cargo.lock [[package]] block's string fields.
var lockField = map[string]*regexp.Regexp{}

func init() {
	for _, key := range []string{"name", "version", "source", "checksum"} {
		lockField[key] = regexp.MustCompile(`(?m)^` + key + ` = "([^"]*)"`)
	}
}

// sha256Hex is a lock file's checksum, as a crates.io package carries one.
var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// fetchFunc returns the bytes at a URL.
type fetchFunc func(ctx context.Context, url string) ([]byte, error)

// crate is one crates.io package a lock file names.
type crate struct {
	name, version, checksum string
	locks                   []string // the lock files naming it
}

// parseLock reads a Cargo.lock: its crates.io packages, and as "<name>
// <version>" the packages it names by path (its own workspace). A package
// from any other registry or from git is refused — this tool can check
// neither.
func parseLock(data []byte) (crates []crate, paths []string, err error) {
	for _, block := range strings.Split(string(data), "[[package]]")[1:] {
		field := func(key string) string {
			m := lockField[key].FindStringSubmatch(block)
			if m == nil {
				return ""
			}
			return m[1]
		}
		c := crate{name: field("name"), version: field("version"), checksum: field("checksum")}
		switch src := field("source"); {
		case c.name == "" || c.version == "":
			return nil, nil, fmt.Errorf("a [[package]] without a name and version:%s", block)
		case src == "":
			paths = append(paths, c.name+" "+c.version)
			continue
		case src != registry:
			return nil, nil, fmt.Errorf("%s %s comes from %s, which this tool cannot check", c.name, c.version, src)
		case !sha256Hex.MatchString(c.checksum):
			return nil, nil, fmt.Errorf("%s %s has no sha256 checksum", c.name, c.version)
		}
		crates = append(crates, c)
	}
	return crates, paths, nil
}

// licenseFile is one license file and what it came from.
type licenseFile struct {
	of   string // "<crate> <version> (<path>)", or a component
	text string
}

// crateLicenses opens a crate archive and returns its Cargo.toml license
// expression and its license files, in path order.
func crateLicenses(archive []byte, c crate) (string, []licenseFile, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return "", nil, fmt.Errorf("%s %s: %w", c.name, c.version, err)
	}
	root := c.name + "-" + c.version + "/"
	var expr string
	var files []licenseFile
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", nil, fmt.Errorf("%s %s: %w", c.name, c.version, err)
		}
		rel, ok := strings.CutPrefix(h.Name, root)
		if !ok || h.Typeflag != tar.TypeReg {
			continue
		}
		if rel == "Cargo.toml" || licenseName.MatchString(path.Base(rel)) {
			b, err := io.ReadAll(tr)
			if err != nil {
				return "", nil, fmt.Errorf("%s %s: %s: %w", c.name, c.version, rel, err)
			}
			if rel == "Cargo.toml" {
				if m := cargoLicense.FindSubmatch(b); m != nil {
					expr = string(m[1])
				}
				continue
			}
			files = append(files, licenseFile{of: fmt.Sprintf("%s %s (%s)", c.name, c.version, rel), text: string(b)})
		}
	}
	slices.SortFunc(files, func(a, b licenseFile) int { return strings.Compare(a.of, b.of) })
	return expr, files, nil
}

// pinned fetches a URL and checks it against its pin.
func pinned(ctx context.Context, fetch fetchFunc, url, want string) ([]byte, error) {
	b, err := fetch(ctx, url)
	if err != nil {
		return nil, err
	}
	if got := digest(b); got != want {
		return nil, fmt.Errorf("%s has sha256 %s, not the pinned %s", url, got, want)
	}
	return b, nil
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// generate renders THIRD_PARTY_LICENSES from the sources, whose own bytes
// hash to sourcesDigest.
func generate(ctx context.Context, fetch fetchFunc, src sources, sourcesDigest string) ([]byte, error) {
	components := map[string]bool{}
	for _, f := range src.Files {
		components[f.Component] = true
	}
	byKey := map[string]*crate{}
	var keys, workspace []string
	for _, l := range src.Lockfiles {
		data, err := pinned(ctx, fetch, l.URL, l.SHA256)
		if err != nil {
			return nil, err
		}
		crates, paths, err := parseLock(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", l.URL, err)
		}
		// A path package's terms are not crates.io's to supply, and not
		// always its project's: each must be put under a component whose
		// texts files carries, and a stale entry is an error too.
		named := map[string]bool{}
		for _, k := range paths {
			c, ok := l.PathPackages[k]
			switch {
			case !ok:
				return nil, fmt.Errorf("%s names %s by path, and its path_packages does not say whose terms it is under", l.Name, k)
			case !components[c]:
				return nil, fmt.Errorf("%s: path_packages puts %s under %q, which files does not carry", l.Name, k, c)
			}
			named[k] = true
			workspace = append(workspace, wrap(fmt.Sprintf("- %s — in %s — under %s", k, l.Name, c), "  "))
		}
		for _, k := range slices.Sorted(maps.Keys(l.PathPackages)) {
			if !named[k] {
				return nil, fmt.Errorf("%s: path_packages names %s, which the lock file does not name by path", l.Name, k)
			}
		}
		for _, c := range crates {
			k := c.name + " " + c.version
			if prev, ok := byKey[k]; ok {
				if prev.checksum != c.checksum {
					return nil, fmt.Errorf("%s has two checksums across the lock files", k)
				}
				prev.locks = append(prev.locks, l.Name)
				continue
			}
			c.locks = []string{l.Name}
			byKey[k] = &c
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for k := range src.CrateFiles {
		if byKey[k] == nil {
			return nil, fmt.Errorf("crate_files names %s, which no lock file does", k)
		}
	}

	var b strings.Builder
	var texts []licenseFile
	header := `THIRD-PARTY LICENSES
====================

managed-agent-platform's executor and worker binaries, and the server image
that carries them, embed ripgrep %[1]s as upstream builds it for Linux
x86_64 and aarch64 (internal/ripgrep), statically linked. This file carries
the license texts of what those builds were made from. NOTICE says what is
embedded and why; LICENSE is this project's own license.

Generated by tools/thirdpartylicenses (make third-party-licenses) from
tools/thirdpartylicenses/sources.json, sha256 %[2]s.
Everything from COMPONENTS on hashes to sha256 %[3]s.
Do not edit it by hand.

The crate list below is a superset. It is every crates.io package named by
ripgrep %[1]s's Cargo.lock and by the Rust standard library's Cargo.lock at
each compiler the two builds were made with — build tools, test-only crates
and other platforms' crates included — because which of them a build links
depends on its target and features, which the generator does not resolve.
Each crate's files come from its crates.io archive, checked against the lock
file's sha256: every file named as a license, copyright, notice or authors
file, at any depth, so a library a crate vendors brings its own (jemalloc's,
in tikv-jemalloc-sys). A crate whose archive carries none takes its
repository's, at the commit the crate was published from, as its text's
heading says. Every other file is fetched from the URL listed, at the sha256
pinned beside it. The packages the lock files name by path, their own
workspaces, are listed under WORKSPACE PACKAGES with the component whose
texts cover each. Every text is as its source has it but for whitespace no
text's meaning rests on: CRLF line ends are made LF, the spaces, tabs and
carriage returns that end a line are dropped, and so are the blank lines that
end a text.

`
	b.WriteString(bodyStart + "\n")
	for _, f := range src.Files {
		data, err := pinned(ctx, fetch, f.URL, f.SHA256)
		if err != nil {
			return nil, err
		}
		line := "- " + f.Component + ": " + f.URL
		if f.Note != "" {
			line += "\n  " + f.Note
		}
		b.WriteString(wrap(line, "  ") + "\n")
		texts = append(texts, licenseFile{of: f.Component + " (" + path.Base(strings.SplitN(f.URL, "?", 2)[0]) + ")", text: string(data)})
	}
	b.WriteString("\nLOCK FILES\n----------\n\n")
	for _, l := range src.Lockfiles {
		b.WriteString(wrap("- "+l.Name+": "+l.URL+"\n  "+l.Note, "  ") + "\n")
	}
	b.WriteString("\nWORKSPACE PACKAGES\n------------------\n\n")
	for _, w := range workspace {
		b.WriteString(w + "\n")
	}

	b.WriteString("\nCRATES\n------\n\n")
	for _, k := range keys {
		c := byKey[k]
		archive, err := pinned(ctx, fetch, crateURL(c.name, c.version), c.checksum)
		if err != nil {
			return nil, err
		}
		expr, files, err := crateLicenses(archive, *c)
		if err != nil {
			return nil, err
		}
		for _, f := range src.CrateFiles[k] {
			data, err := pinned(ctx, fetch, f.URL, f.SHA256)
			if err != nil {
				return nil, err
			}
			files = append(files, licenseFile{of: k + " (" + f.URL + ")", text: string(data)})
		}
		// A crate with no text is an error: its terms would be a claim this
		// file could not back.
		if len(files) == 0 {
			return nil, fmt.Errorf("%s ships no license file and crate_files supplies none", k)
		}
		b.WriteString(wrap(fmt.Sprintf("- %s %s — %s — in %s", c.name, c.version, expr, strings.Join(c.locks, "; ")), "  ") + "\n")
		texts = append(texts, files...)
	}

	// One copy of each distinct text, naming everything that ships it.
	b.WriteString("\nLICENSE TEXTS\n-------------\n")
	var order []string
	who := map[string][]string{}
	body := map[string]string{}
	for _, t := range texts {
		norm := normText(t.text)
		k := digest([]byte(norm))
		if _, ok := body[k]; !ok {
			order = append(order, k)
			body[k] = norm
		}
		who[k] = append(who[k], t.of)
	}
	rule := strings.Repeat("=", 78)
	for i, k := range order {
		fmt.Fprintf(&b, "\n%s\n%s\n%s\n\n%s", rule, wrap(fmt.Sprintf("%d. %s", i+1, strings.Join(who[k], ", ")), "   "), rule, body[k])
	}
	return []byte(fmt.Sprintf(header, src.Ripgrep, sourcesDigest, digest([]byte(b.String()))) + b.String()), nil
}

// normText is a license text as the file carries it: CRLF line ends made LF,
// the spaces, tabs and carriage returns that end each line dropped, and the
// blank lines that end the text, and one newline to end it. None of that is
// any text's meaning, and what is left has no whitespace git counts as
// trailing; two texts that differ only there are one.
func normText(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t\r")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n"
}

// bodyStart opens the part of the file whose sha256 the header records.
const bodyStart = "COMPONENTS\n----------\n"

// wrap breaks each line of s at spaces to fit 78 columns, continuing a broken
// line with indent.
func wrap(s, indent string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		for len(line) > 78 {
			cut := strings.LastIndex(line[:79], " ")
			if cut <= len(indent) {
				break
			}
			out = append(out, line[:cut])
			line = indent + strings.TrimLeft(line[cut:], " ")
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// attempts is how often a download is tried before the run fails, as
// tools/ripgrepfetch tries.
const attempts = 3

func httpFetch(client *http.Client) fetchFunc {
	return func(ctx context.Context, url string) ([]byte, error) {
		var last error
		for i := range attempts {
			if i > 0 {
				time.Sleep(time.Duration(i) * 2 * time.Second)
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return nil, err
			}
			resp, err := client.Do(req)
			if err != nil {
				last = err
				continue
			}
			b, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
			resp.Body.Close()
			switch {
			case err != nil:
				last = err
			case resp.StatusCode != http.StatusOK:
				last = fmt.Errorf("GET %s: %s", url, resp.Status)
			case len(b) > maxDownload:
				return nil, fmt.Errorf("GET %s: larger than %d bytes", url, maxDownload)
			default:
				return b, nil
			}
		}
		return nil, last
	}
}

func main() {
	log.SetFlags(0)
	in := flag.String("sources", "tools/thirdpartylicenses/sources.json", "the pinned inputs")
	out := flag.String("out", "THIRD_PARTY_LICENSES", "the file to write")
	flag.Parse()
	raw, err := os.ReadFile(*in)
	if err != nil {
		log.Fatalf("thirdpartylicenses: %v", err)
	}
	var src sources
	if err := json.Unmarshal(raw, &src); err != nil {
		log.Fatalf("thirdpartylicenses: %s: %v", *in, err)
	}
	if src.Ripgrep == "" || len(src.Lockfiles) == 0 || len(src.Files) == 0 {
		log.Fatalf("thirdpartylicenses: %s: %v", *in, errors.New("ripgrep, lockfiles and files are all required"))
	}
	text, err := generate(context.Background(), httpFetch(&http.Client{Timeout: 2 * time.Minute}), src, digest(raw))
	if err != nil {
		log.Fatalf("thirdpartylicenses: %v", err)
	}
	if err := os.WriteFile(*out, text, 0o644); err != nil {
		log.Fatalf("thirdpartylicenses: %v", err)
	}
	log.Printf("wrote %s (%d bytes)", *out, len(text))
}
