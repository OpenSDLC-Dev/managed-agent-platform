package api

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// maxFileBytes is the public docs' per-file cap (500 MB). A package var, not a
// const, so export_test.go can lower it to exercise the 413 path without
// streaming half a gigabyte through a test. Self-hosted operators own their
// disk, so the reference's 500 GB per-org quota is deliberately not enforced
// (docs/DIVERGENCES.md).
var maxFileBytes int64 = 500 << 20

// fileUploadHeadroom is the multipart-framing slop added to the total-body
// MaxBytesReader budget, so a file exactly at maxFileBytes is not tipped over
// by the part boundary. The per-file cap itself is enforced on the part content
// (below), independent of this whole-body defense.
const fileUploadHeadroom = 1 << 20

// fileUpload is a decoded single-file multipart upload.
type fileUpload struct {
	filename string
	mimeType string
	data     []byte
}

// parseFileUpload reads a multipart/form-data body carrying exactly one part
// named "file" (BetaFileUploadParams: the SDK emits one `file` part). The
// filename comes from the part's Content-Disposition and is validated against
// the documented rules; the MIME type is taken from the part header, falling
// back to the filename extension. Extra, unknown, or duplicate parts are
// rejected — the reference's strictness here is unrecorded, so this is an
// inference (docs/DIVERGENCES.md).
func parseFileUpload(r *http.Request) (*fileUpload, error) {
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		return nil, errInvalid("request must be multipart/form-data with one file part")
	}
	// Whole-body defense: MaxBytesReader stops a giant connection from being
	// read at all (typed 413 mid-read). The per-file cap is enforced on the
	// part content below — this budget is that cap plus framing headroom.
	body := http.MaxBytesReader(nil, r.Body, maxFileBytes+fileUploadHeadroom)
	mr := multipart.NewReader(body, params["boundary"])
	var up *fileUpload
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, mapFileBodyErr(err)
		}
		if part.FormName() != "file" {
			return nil, errInvalid("unknown form field %q; send one file part named \"file\"", part.FormName())
		}
		if up != nil {
			return nil, errInvalid("duplicate file part; send exactly one")
		}
		filename := rawPartFilename(part)
		if err := validateFilename(filename); err != nil {
			return nil, err
		}
		// Bound the part content to exactly the per-file cap: read one byte past
		// it, and reject if that byte exists. This enforces the documented cap on
		// the file itself, independent of the framing headroom in the body budget.
		data, err := io.ReadAll(io.LimitReader(part, maxFileBytes+1))
		if err != nil {
			return nil, mapFileBodyErr(err)
		}
		if int64(len(data)) > maxFileBytes {
			return nil, &apiError{http.StatusRequestEntityTooLarge, errTypeRequestTooLarge,
				fmt.Sprintf("file larger than %d bytes", maxFileBytes)}
		}
		up = &fileUpload{filename: filename, mimeType: fileMimeType(part, filename), data: data}
	}
	if up == nil {
		return nil, errInvalid(`no file uploaded: send one part named "file"`)
	}
	return up, nil
}

// forbiddenFilenameChars are the characters the public Files docs reject in a
// filename: the Windows-reserved set plus both path separators (so a
// path-qualified name is rejected — a filename is a bare basename).
const forbiddenFilenameChars = `<>:"|?*\/`

// validateFilename enforces the documented rule: 1–255 characters, none of the
// forbidden set, no "Unicode characters 0-31" (U+0000–U+001F, exactly — the
// public docs' wording, so DEL and the C1 range are deliberately not rejected).
// Length is counted in runes, not bytes, per "1-255 characters". A filename
// with invalid UTF-8 is rejected too: it would fail as a 500 at the text-column
// bind, the #135 class. The exact wire error text is an inference
// (docs/DIVERGENCES.md).
func validateFilename(name string) error {
	if name == "" {
		return errInvalid("file part is missing a filename")
	}
	if !utf8.ValidString(name) {
		return errInvalid("filename must be valid UTF-8")
	}
	if utf8.RuneCountInString(name) > 255 {
		return errInvalid("filename must be between 1 and 255 characters")
	}
	if strings.ContainsAny(name, forbiddenFilenameChars) {
		return errInvalid(`filename must not contain any of %s`, forbiddenFilenameChars)
	}
	for _, r := range name {
		if r < 0x20 {
			return errInvalid("filename must not contain control characters (U+0000–U+001F)")
		}
	}
	return nil
}

// fileMimeType resolves the stored MIME type: the part's declared Content-Type
// when it is specific, otherwise the filename extension, otherwise the generic
// octet-stream. The reference's exact derivation is unrecorded — an inference
// (docs/DIVERGENCES.md). The part Content-Type is a raw header value that can
// carry a non-UTF-8 byte; it is used only when storable, so a malformed one
// falls through to the extension rather than 500ing at the text-column bind
// (the #135 class, guarded on filename above and on scope_id/cursor in files.go).
func fileMimeType(part *multipart.Part, filename string) string {
	ct := part.Header.Get("Content-Type")
	if ct != "" && ct != "application/octet-stream" && storableText(ct) {
		return ct
	}
	if byExt := mime.TypeByExtension(filepath.Ext(filename)); byExt != "" {
		return byExt
	}
	return "application/octet-stream"
}

// mapFileBodyErr turns a body-read failure into the wire error: the
// MaxBytesReader budget as a 413, anything else as a malformed multipart body.
func mapFileBodyErr(err error) error {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return &apiError{http.StatusRequestEntityTooLarge, errTypeRequestTooLarge,
			fmt.Sprintf("file larger than %d bytes", maxFileBytes)}
	}
	return errInvalid("malformed multipart body")
}
