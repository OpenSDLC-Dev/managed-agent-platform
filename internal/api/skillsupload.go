package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
// One files[] part that is a zip archive (by magic bytes — where the reference
// goes by the filename's extension, a recorded mismatch in
// docs/DIVERGENCES.md) is the zip form; anything else is the
// loose path-qualified form. A refusal is the skills package's own error, for
// the caller to answer through refuse.
func (u *skillUpload) bundle() (*skills.Bundle, error) {
	if len(u.files) == 1 && skills.IsZip(u.files[0].Data) {
		return skills.FromZip(u.files[0].Data)
	}
	return skills.FromFiles(u.files)
}

// refuse logs a refusal from bundle and returns the 400 it answers. A
// frontmatter length refusal answers the reference's sentence, recorded for
// both fields on both routes (2026-09-12-followups skills-api.json #8, #9,
// #18, #19; #540); it names neither the field nor its cap, which the line's
// reason does. Every refusal carries `x-should-retry: false`, as the bundle
// refusals recorded where the header was captured do (2026-09-05 batch1
// `SKILL.md not found in uploaded files` and the unreadable archive; #842).
func (u *skillUpload) refuse(ctx context.Context, err error, attrs ...any) error {
	attrs = append(attrs, "request_id", requestIDFrom(ctx),
		"files", len(u.files), "bytes", u.totalBytes(), "reason", err)
	slog.InfoContext(ctx, "skill upload rejected", attrs...)
	var tooLong *skills.LengthError
	if errors.As(err, &tooLong) {
		return noRetry(errInvalid("`name` and `description` must resolve from `SKILL.md` frontmatter or its fallbacks, within their length limits"))
	}
	return noRetry(errInvalid("%s", err))
}

// parseSkillUpload reads a multipart/form-data body of files[] parts (plus
// display_name on the create form).
//
// Unknown parts are IGNORED rather than rejected (plan 39, decision 8). The
// recording shows a create carrying a stray display_title part succeeding with
// its name derived from the frontmatter, exactly as if the part were absent.
// Two further names, description and xyzzy, were later put through both routes
// and ignored the same way. Three names are still a sample, so tolerating any
// unknown part remains an extrapolation — a weaker one than decision 8 made
// from display_title alone, and the registry says so.
//
// files[] stays required. A files[] part without a filename is still rejected,
// as it is on the reference — a 400 there too, in its words, recorded in
// docs/DIVERGENCES.md.
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
				// The reference's validator words a part with no filename as
				// a string where a file was due (2026-09-12-followups
				// skills-api.json #2, #12; #540). Its index is the part's
				// 0-based position among the files[] parts; only index 0 was
				// recorded, so that counting is an inference.
				return nil, errInvalid("files[].%d: Expected UploadFile, received: <class 'str'>", len(up.files))
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
				return nil, noRetry(errInvalid("display_name must be at most %d characters long", maxDisplayNameChars))
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
		return nil, noRetry(errInvalid("files[]: Field required"))
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
