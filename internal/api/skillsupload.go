package api

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/skills"
)

// maxSkillBodyBytes bounds skill uploads — the one surface that carries
// payloads rather than configuration, so it gets its own budget beside
// decodeObject's maxBodyBytes: the published 30 MB skill cap plus headroom
// for multipart framing.
const maxSkillBodyBytes = 32 << 20

// maxDisplayTitleBytes bounds the one plain-text form field.
const maxDisplayTitleBytes = 4 << 10

// skillUpload is a decoded multipart skill upload, one entry per files[]
// part. Paths are the raw path-qualified filenames the client sent.
type skillUpload struct {
	displayTitle    string
	displayTitleSet bool
	files           []skills.File
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
// display_title on the create form). Unknown fields are rejected like
// decodeObject's unknown keys; a files[] part without a filename is rejected
// (the reference's tolerance is unrecorded — docs/DIVERGENCES.md).
func parseSkillUpload(r *http.Request, allowDisplayTitle bool) (*skillUpload, error) {
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
		switch part.FormName() {
		case "files[]":
			filename := rawPartFilename(part)
			if filename == "" {
				return nil, errInvalid("files[] part is missing a filename")
			}
			data, err := io.ReadAll(part)
			if err != nil {
				return nil, mapSkillBodyErr(err)
			}
			up.files = append(up.files, skills.File{Path: filename, Data: data})
		case "display_title":
			if !allowDisplayTitle {
				return nil, errInvalid(`unknown form field "display_title"`)
			}
			if up.displayTitleSet {
				return nil, errInvalid("duplicate display_title field")
			}
			data, err := io.ReadAll(io.LimitReader(part, maxDisplayTitleBytes+1))
			if err != nil {
				return nil, mapSkillBodyErr(err)
			}
			if len(data) > maxDisplayTitleBytes {
				return nil, errInvalid("display_title is longer than %d bytes", maxDisplayTitleBytes)
			}
			up.displayTitle = string(data)
			up.displayTitleSet = true
		default:
			return nil, errInvalid("unknown form field %q", part.FormName())
		}
	}
	if len(up.files) == 0 {
		return nil, errInvalid("no files uploaded: send one files[] part per file")
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
