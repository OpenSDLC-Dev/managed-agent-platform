package ripgrep

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

// The manifest is a pin, held to the shape the repository pins everything in
// (.github/dependabot.yml's rule for actions, which pins_test.py enforces
// there): an immutable identity beside a name a reader can review. Here that
// is upstream's own release URL, whose path carries the version, and the
// archive's sha256 — and the archive must be upstream's musl build for the
// architecture, because that is the static one.
func TestManifestPinsUpstreamMuslArchives(t *testing.T) {
	m := Pinned
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(m.Version) {
		t.Fatalf("version %q is not a three-part release", m.Version)
	}
	triples := map[string]string{"amd64": "x86_64-unknown-linux-musl", "arm64": "aarch64-unknown-linux-musl"}
	if len(m.Archives) != len(triples) {
		t.Fatalf("archives = %d, want exactly %d (linux amd64 and arm64)", len(m.Archives), len(triples))
	}
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for arch, triple := range triples {
		a, ok := m.Archives[arch]
		if !ok {
			t.Errorf("no archive for %s", arch)
			continue
		}
		want := "https://github.com/BurntSushi/ripgrep/releases/download/" + m.Version + "/ripgrep-" + m.Version + "-" + triple + ".tar.gz"
		if a.URL != want {
			t.Errorf("%s url = %q, want %q", arch, a.URL, want)
		}
		if !hex64.MatchString(a.SHA256) {
			t.Errorf("%s sha256 = %q, want 64 lowercase hex digits", arch, a.SHA256)
		}
	}
}

// What the gate embeds is what runs in a sandbox: for each architecture an ELF
// for that machine with no program interpreter — statically linked, so it
// needs nothing from the image it lands in. This needs the fetched archives,
// as everything that builds a binary does; `make test` fetches them first.
func TestOpenReturnsStaticLinuxBinaries(t *testing.T) {
	for arch, machine := range map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64} {
		rg, size, err := Open(arch)
		if errors.Is(err, ErrNotEmbedded) {
			t.Fatalf("Open(%s): %v — run `make ripgrep` first", arch, err)
		}
		if err != nil {
			t.Fatalf("Open(%s): %v", arch, err)
		}
		b, err := io.ReadAll(rg)
		if err != nil || int64(len(b)) != size {
			t.Fatalf("Open(%s) read %d of %d bytes: %v", arch, len(b), size, err)
		}
		f, err := elf.NewFile(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("Open(%s): not an ELF: %v", arch, err)
		}
		if f.Machine != machine || f.Class != elf.ELFCLASS64 {
			t.Errorf("Open(%s) is a %v %v binary, want %v", arch, f.Class, f.Machine, machine)
		}
		for _, p := range f.Progs {
			if p.Type == elf.PT_INTERP {
				t.Errorf("Open(%s) names a program interpreter: it is dynamically linked", arch)
			}
		}
	}
}

// archive builds a release-shaped archive holding the given members.
func archive(t *testing.T, members map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range members {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const fakeArchive = "ripgrep-9.9.9-x86_64-unknown-linux-musl.tar.gz"

// pinning is a manifest that pins sum as the amd64 archive.
func pinning(sum string) Manifest {
	return Manifest{Version: "9.9.9", Archives: map[string]Archive{
		"amd64": {URL: "https://example.invalid/" + fakeArchive, SHA256: sum},
	}}
}

// holding is an assets tree holding data as the amd64 archive, or nothing.
func holding(data []byte) fstest.MapFS {
	fsys := fstest.MapFS{"assets/manifest.json": {Data: []byte("{}")}}
	if data != nil {
		fsys["assets/"+fakeArchive] = &fstest.MapFile{Data: data}
	}
	return fsys
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Open hands out only the pinned archive's rg: a build that fetched nothing
// says so, and an archive whose bytes are not the pin's — or that is not the
// archive it is named as — is refused before anything is read out of it.
func TestOpenRefusesWhatThePinDoesNot(t *testing.T) {
	good := archive(t, map[string]string{
		"ripgrep-9.9.9-x86_64-unknown-linux-musl/README.md": "readme",
		"ripgrep-9.9.9-x86_64-unknown-linux-musl/rg":        "\x7fELF-rg",
	})
	rg, size, err := open(holding(good), pinning(digest(good)), "amd64")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if b, _ := io.ReadAll(rg); string(b) != "\x7fELF-rg" || size != int64(len(b)) {
		t.Fatalf("open = %q (%d bytes)", b, size)
	}

	for name, tc := range map[string]struct {
		fsys fstest.MapFS
		pin  Manifest
		arch string
		want string
	}{
		"nothing fetched":         {holding(nil), pinning(digest(good)), "amd64", ErrNotEmbedded.Error()},
		"an unpinned arch":        {holding(good), pinning(digest(good)), "riscv64", "no archive for linux/riscv64"},
		"bytes that are not pins": {holding(append(good, 0)), pinning(digest(good)), "amd64", "does not match its pinned sha256"},
		"not gzip":                {holding([]byte("plain text")), pinning(digest([]byte("plain text"))), "amd64", "gzip: invalid header"},
		"no rg inside": {func() fstest.MapFS { return holding(archive(t, map[string]string{"other/rg": "x"})) }(),
			pinning(digest(archive(t, map[string]string{"other/rg": "x"}))), "amd64", "holds no ripgrep-9.9.9-x86_64-unknown-linux-musl/rg"},
	} {
		_, _, err := open(tc.fsys, tc.pin, tc.arch)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, tc.want)
		}
	}
}

// Check is Open over every pinned archive, one architecture at a time:
// nothing for a build carrying them all, and otherwise each architecture that
// cannot be opened, named, with why — every one, for a build that fetched
// none, which is the case the startup warning exists for.
func TestCheckOpensEveryPinnedArchive(t *testing.T) {
	if bad := Check(); len(bad) != 0 {
		t.Fatalf("Check: %v — run `make ripgrep` first", bad)
	}
	good := archive(t, map[string]string{"ripgrep-9.9.9-x86_64-unknown-linux-musl/rg": "\x7fELF-rg"})
	if bad := check(holding(good), pinning(digest(good))); len(bad) != 0 {
		t.Errorf("check over a good archive: %v", bad)
	}
	// Two architectures pinned, the arm64 archive never fetched and the
	// amd64 one present: only arm64 is named.
	both := pinning(digest(good))
	both.Archives["arm64"] = Archive{URL: "https://example.invalid/ripgrep-9.9.9-aarch64-unknown-linux-musl.tar.gz", SHA256: digest(good)}
	if bad := check(holding(good), both); len(bad) != 1 || bad[0].Arch != "arm64" || !errors.Is(bad[0].Err, ErrNotEmbedded) {
		t.Errorf("check with arm64 missing = %v, want arm64 alone, not embedded", bad)
	}
	// Neither fetched: both named, in architecture order.
	bad := check(holding(nil), both)
	if len(bad) != 2 || bad[0].Arch != "amd64" || bad[1].Arch != "arm64" || !errors.Is(bad[0].Err, ErrNotEmbedded) || !errors.Is(bad[1].Err, ErrNotEmbedded) {
		t.Errorf("check over nothing fetched = %v, want amd64 and arm64, not embedded", bad)
	}
	// A wrong archive is named with its own error.
	if bad := check(holding(append(good, 0)), pinning(digest(good))); len(bad) != 1 || bad[0].Arch != "amd64" ||
		!strings.Contains(bad[0].Err.Error(), "does not match its pinned sha256") {
		t.Errorf("check over a wrong archive = %v", bad)
	}
}

// The embedded manifest is the file in the tree, read whole: a field it
// carries that Manifest does not would be a pin nothing enforces.
func TestPinnedIsTheManifestFile(t *testing.T) {
	var raw map[string]any
	if err := json.Unmarshal(manifestJSON, &raw); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(Pinned)
	var back map[string]any
	_ = json.Unmarshal(b, &back)
	if !reflect.DeepEqual(raw, back) {
		t.Fatalf("assets/manifest.json = %v, but Manifest reads %v", raw, back)
	}
}

// The repository's NOTICE carries ripgrep's license into every artifact that
// embeds it, names the release it describes — a pin moved without it is a
// notice describing a binary nobody ships — and points at the file carrying
// the license texts of what that release links (tools/thirdpartylicenses
// holds that file to the pin).
func TestNoticeNamesThePinnedRelease(t *testing.T) {
	b, err := os.ReadFile("../../NOTICE")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ripgrep " + Pinned.Version + " ", "Copyright (c) 2015 Andrew Gallant", "x86_64-unknown-linux-musl", "aarch64-unknown-linux-musl", "THIRD_PARTY_LICENSES"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("NOTICE does not mention %q", want)
		}
	}
}
