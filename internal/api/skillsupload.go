package api

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"unicode/utf8"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/skills"
)

// maxSkillBodyBytes bounds skill uploads — the one surface that carries
// payloads rather than configuration, so it gets its own budget beside
// decodeObject's maxBodyBytes: the published 30 MB skill cap plus headroom
// for multipart framing.
const maxSkillBodyBytes = 32 << 20

// maxDisplayNameChars bounds the one plain-text form field. 255 is the
// reference's own cap and characters is its own unit: the 2026-09-04 recording
// accepts a 255-character display_name and refuses a 256-character one with the
// message reproduced below (plan 39, decision 7). Counting bytes instead would
// refuse a 100-character CJK name the reference accepts, and say something
// false about it while doing so.
const maxDisplayNameChars = 255

// skillUpload is a decoded multipart skill upload, one entry per files[]
// part. Paths are the raw path-qualified filenames the client sent.
type skillUpload struct {
	displayName    string
	displayNameSet bool
	files          []skills.File
}

// totalBytes is the received content size, for the upload metrics.
func (u *skillUpload) totalBytes() int64 {
	var n int64
	for _, f := range u.files {
		n += int64(len(f.Data))
	}
	return n
}

// bundle validates the upload and normalizes it to the canonical archive.
// One files[] part that is a zip archive (by magic bytes — an inference
// recorded in docs/DIVERGENCES.md) is the zip form; anything else is the
// loose path-qualified form.
func (u *skillUpload) bundle() (*skills.Bundle, error) {
	if len(u.files) == 1 && skills.IsZip(u.files[0].Data) {
		b, err := skills.FromZip(u.files[0].Data)
		if err != nil {
			return nil, errInvalid("%s", err)
		}
		return b, nil
	}
	b, err := skills.FromFiles(u.files)
	if err != nil {
		return nil, errInvalid("%s", err)
	}
	return b, nil
}

// parseSkillUpload reads a multipart/form-data body of files[] parts (plus
// display_name on the create form).
//
// Unknown parts are IGNORED rather than rejected (plan 39, decision 8). The
// recording shows a create carrying a stray display_title part succeeding with
// its name derived from the frontmatter, exactly as if the part were absent —
// which is only true if unknown parts in general are tolerated. The observed
// evidence covers that one field name; the registry entry says so.
//
// files[] stays required. A files[] part without a filename is still rejected
// (the reference's tolerance there is unrecorded — docs/DIVERGENCES.md).
func parseSkillUpload(r *http.Request, allowDisplayName bool) (*skillUpload, error) {
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		return nil, errInvalid("request must be multipart/form-data with one files[] part per file")
	}
	// MaxBytesReader (not LimitReader) so an over-budget upload surfaces as a
	// typed error mid-read instead of a truncated-archive parse failure.
	body := http.MaxBytesReader(nil, r.Body, maxSkillBodyBytes)
	mr := multipart.NewReader(body, params["boundary"])
	var up skillUpload
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, mapSkillBodyErr(err)
		}
		switch name := part.FormName(); {
		case name == "files[]":
			filename := rawPartFilename(part)
			if filename == "" {
				return nil, errInvalid("files[] part is missing a filename")
			}
			data, err := io.ReadAll(part)
			if err != nil {
				return nil, mapSkillBodyErr(err)
			}
			up.files = append(up.files, skills.File{Path: filename, Data: data})
		case name == "display_name" && allowDisplayName:
			if up.displayNameSet {
				return nil, errInvalid("duplicate display_name field")
			}
			// The reader admits a character's widest encoding, because a
			// bound counted in characters cannot be applied to bytes already
			// truncated: cut at 255 of them, a multi-byte name over the cap
			// would arrive under it.
			data, err := io.ReadAll(io.LimitReader(part, maxDisplayNameChars*utf8.UTFMax+1))
			if err != nil {
				return nil, mapSkillBodyErr(err)
			}
			if utf8.RuneCount(data) > maxDisplayNameChars {
				return nil, errInvalid("display_name must be at most %d characters long", maxDisplayNameChars)
			}
			up.displayName = string(data)
			up.displayNameSet = true
		}
		// Every other part is ignored, display_name on the version form (which
		// defines no such field) included.
	}
	if len(up.files) == 0 {
		// The reference's own wording, and also what a part named bare "files"
		// gets: it is not files[], so it is ignored, and the form then carries
		// no file part at all.
		return nil, errInvalid("files[]: Field required")
	}
	return &up, nil
}

// rawPartFilename returns the part's Content-Disposition filename exactly as
// sent. Part.FileName is unusable here: it passes the value through
// filepath.Base, which would strip the path qualification the loose-files
// upload form is defined by.
func rawPartFilename(p *multipart.Part) string {
	_, params, err := mime.ParseMediaType(p.Header.Get("Content-Disposition"))
	if err != nil {
		return ""
	}
	return params["filename"]
}

// mapSkillBodyErr turns a body-read failure into the wire error: the
// MaxBytesReader budget as the 413 decodeObject's oversize path uses,
// anything else as a malformed multipart body.
func mapSkillBodyErr(err error) error {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return &apiError{http.StatusRequestEntityTooLarge, errTypeRequestTooLarge,
			fmt.Sprintf("request body larger than %d bytes", maxSkillBodyBytes)}
	}
	return errInvalid("malformed multipart body")
}
