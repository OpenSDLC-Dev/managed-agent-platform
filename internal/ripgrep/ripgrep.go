// Package ripgrep carries the static ripgrep the grep tool runs inside a
// sandbox (#827, an owner decision): one Linux binary per sandbox
// architecture, embedded in every executor and worker build, which the
// toolset writes into a sandbox the first time it greps there.
//
// The binaries are upstream's own release archives, unmodified, pinned in
// assets/manifest.json by URL and sha256 — the musl builds, which are
// statically linked (x86_64 as static-pie), so one file runs on any Linux
// userland: glibc, musl, busybox, or none beyond a shell. Upstream publishes
// a musl archive for both architectures since 15.0.0, so nothing is built
// here. The archives are embedded still compressed, about 2 MB each, and
// opened in memory when a sandbox needs one.
//
// They are not committed. `make ripgrep` (tools/ripgrepfetch) downloads the
// pinned archives into assets/, checking each digest, and every target that
// builds something that runs grep — the test gate, the eval suite, the worker
// release tarballs, the Dockerfile's build stage — runs it first. A build that
// skipped it still compiles, because the manifest alone satisfies the embed,
// and Open then answers ErrNotEmbedded: grep is a tool error in that build,
// never a second implementation. Open re-checks the digest on every call, so a
// stray file under the pinned name cannot be embedded and run in its place.
//
// ripgrep is dual-licensed MIT and Unlicense; NOTICE at the repository root
// carries its license for the artifacts that embed it, and
// THIRD_PARTY_LICENSES the license texts of everything those static builds
// link (tools/thirdpartylicenses).
package ripgrep

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"
)

// The manifest and the archives are two embeds although one would hold both:
// a package-level reader of the archives keeps them in every binary that
// imports this package, and reading the manifest at init would be one. Kept
// apart, a binary links the archives only if it can reach Open — the executor
// and the worker, and not the brain or the control plane, which import the
// toolset for its tool definitions alone.
var (
	//go:embed assets/manifest.json
	manifestJSON []byte
	//go:embed assets
	assets embed.FS
)

// ErrNotEmbedded is Open's answer in a build whose assets were never fetched.
var ErrNotEmbedded = errors.New("this build carries no ripgrep (it was built without `make ripgrep`)")

// Archive is one architecture's pinned release archive.
type Archive struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// Name is the archive's file name: the last element of its URL, which is also
// the name it is fetched to and embedded under.
func (a Archive) Name() string { return path.Base(a.URL) }

// member is the archive's path to the binary. Upstream lays every archive out
// as one directory named after the archive, holding rg.
func (a Archive) member() string { return strings.TrimSuffix(a.Name(), ".tar.gz") + "/rg" }

// Manifest is assets/manifest.json: the ripgrep version, as `rg --version`
// prints it, and by Go architecture name the archive that carries it.
type Manifest struct {
	Version  string             `json:"version"`
	Archives map[string]Archive `json:"archives"`
}

// Pinned is the embedded manifest. It is read once, at init, like a
// regexp.MustCompile: the file is compiled in, so a malformed one fails every
// test that imports this package rather than a call in production.
var Pinned = mustLoadManifest()

func mustLoadManifest() Manifest {
	var m Manifest
	if err := json.Unmarshal(manifestJSON, &m); err != nil {
		panic(fmt.Errorf("ripgrep: assets/manifest.json: %w", err))
	}
	return m
}

// Open returns the rg binary for a Linux GOARCH ("amd64" or "arm64") as a
// stream of exactly size bytes, decompressed from the embedded archive after
// the archive's digest has been checked against the manifest.
func Open(goarch string) (rg io.Reader, size int64, err error) { return open(assets, Pinned, goarch) }

// Check opens every pinned archive as an install would, and answers the
// first that cannot be — ErrNotEmbedded in a build made without `make
// ripgrep`. The executor and the worker call it at startup, so a build whose
// grep can only answer with a tool error says so in its log before a model
// finds out.
func Check() error { return check(assets, Pinned) }

func check(fsys fs.FS, m Manifest) error {
	for _, arch := range slices.Sorted(maps.Keys(m.Archives)) {
		if _, _, err := open(fsys, m, arch); err != nil {
			return err
		}
	}
	return nil
}

func open(fsys fs.FS, m Manifest, goarch string) (io.Reader, int64, error) {
	a, ok := m.Archives[goarch]
	if !ok {
		return nil, 0, fmt.Errorf("ripgrep: no archive for linux/%s", goarch)
	}
	data, err := fs.ReadFile(fsys, "assets/"+a.Name())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, ErrNotEmbedded
	}
	if err != nil {
		return nil, 0, err
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != a.SHA256 {
		return nil, 0, fmt.Errorf("ripgrep: the embedded %s does not match its pinned sha256 %s", a.Name(), a.SHA256)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, 0, fmt.Errorf("ripgrep: %s: %w", a.Name(), err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, 0, fmt.Errorf("ripgrep: %s holds no %s", a.Name(), a.member())
		}
		if err != nil {
			return nil, 0, fmt.Errorf("ripgrep: %s: %w", a.Name(), err)
		}
		if h.Name == a.member() && h.Typeflag == tar.TypeReg {
			return tr, h.Size, nil
		}
	}
}
