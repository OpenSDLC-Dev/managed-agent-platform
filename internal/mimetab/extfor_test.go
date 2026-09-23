package mimetab

import (
	"mime"
	"testing"
)

func TestExtFor(t *testing.T) {
	cases := map[string]string{
		"application/pdf":           ".pdf",
		"image/png":                 ".png",
		"image/jpeg":                ".jpg",
		"IMAGE/JPEG":                ".jpg", // case-folded
		"text/plain":                ".txt",
		"text/plain; charset=utf-8": ".txt", // parameters ignored
		"application/octet-stream":  ".bin",
		"application/x-unlisted":    "",
		"":                          "",
		"not a media type;;":        "",
	}
	for mt, want := range cases {
		if got := ExtFor(mt); got != want {
			t.Errorf("ExtFor(%q) = %q, want %q", mt, got, want)
		}
	}
}

// Every listed type names a file ByPath maps straight back to the same value,
// so the name a nameless upload is given always maps back to its stored
// mime_type's bare media type.
func TestExtForRoundTrips(t *testing.T) {
	for _, full := range byExt {
		ext := ExtFor(full)
		if ext == "" {
			t.Errorf("ExtFor(%q) = \"\", want the extension the table lists it under", full)
			continue
		}
		if got := ByPath("unnamed" + ext); got != full {
			t.Errorf("ByPath(unnamed%s) = %q, want %q", ext, got, full)
		}
	}
}

// A type the table lists under several extensions must name its preference,
// or map iteration order would choose the name.
func TestAmbiguousTypesArePinned(t *testing.T) {
	exts := map[string][]string{}
	for ext, full := range byExt {
		mt, _, _ := mime.ParseMediaType(full)
		exts[mt] = append(exts[mt], ext)
	}
	for mt, list := range exts {
		if len(list) < 2 {
			continue
		}
		pref, ok := preferredExt[mt]
		if !ok {
			t.Errorf("%s is listed under %v and has no preferredExt entry", mt, list)
			continue
		}
		if byExt[pref] == "" {
			t.Errorf("preferredExt[%s] = %q, which the table does not list", mt, pref)
		}
	}
}
