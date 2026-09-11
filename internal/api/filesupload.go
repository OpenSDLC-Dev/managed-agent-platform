package api

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/mimetab"
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

// The documented bounds on expires_in_seconds: "an integer number of seconds
// between 3,600 (1 hour) and 7,776,000 (90 days)", inclusive at both ends (the
// API reference states them as minimum and maximum). They are enforced here,
// where a bad value is a wire 400, rather than as a CHECK on the column, where
// it would be a 500 at the bind (migration 0037 argues that in place).
const (
	minExpiresInSeconds = 3600
	maxExpiresInSeconds = 7776000
	// maxExpiresInBytes bounds the part before it is parsed at all: the field
	// is a number, and the longest one that could ever be in range is seven
	// digits. The slack is for a sign and for a client that pads.
	maxExpiresInBytes = 32
)

// fileUpload is a decoded single-file multipart upload.
type fileUpload struct {
	filename string
	mimeType string
	data     []byte
	// expiresIn is the requested lifetime in seconds, nil when the upload sent
	// no expires_in_seconds part. It stays a lifetime rather than an instant
	// all the way to the INSERT: the docs define expires_at as "the upload time
	// plus that value", the upload time is the created_at Postgres stamps, and
	// computing it here would substitute this process's clock for that one.
	expiresIn *int64
}

// parseFileUpload reads a multipart/form-data body carrying exactly one part
// named "file" (BetaFileUploadParams: the SDK emits one `file` part) and at
// most one named "expires_in_seconds". The filename comes from the part's
// Content-Disposition and is validated against the documented rules; the MIME
// type is taken from the part header, falling back to the filename extension.
// Extra, unknown, or duplicate parts are rejected — the reference's strictness
// here is unrecorded, so this is an inference (docs/DIVERGENCES.md).
//
// The two parts may arrive in either order, because nothing decides one for a
// client: the SDK's encoder happens to write the file first, curl writes the
// -F flags in the order given, and a hand-rolled form writes whatever it
// writes. RFC 7578 §5.2 asks a sender to preserve its form's order and an
// intermediary not to reorder — neither of which tells this parser what order
// to expect — so it depends on none.
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
	var expiresIn *int64
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, mapFileBodyErr(err)
		}
		if part.FormName() == "expires_in_seconds" {
			if expiresIn != nil {
				return nil, errInvalid("duplicate expires_in_seconds part; send at most one")
			}
			secs, err := parseExpiresIn(part)
			if err != nil {
				return nil, err
			}
			expiresIn = &secs
			continue
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
	up.expiresIn = expiresIn
	return up, nil
}

// parseExpiresIn reads the expires_in_seconds part: an integer in the
// documented range. The exact wire error the reference gives for a bad value is
// unrecorded — the docs publish the bounds and not the message — so the wording
// is ours (docs/DIVERGENCES.md).
//
// The value is bounded before it is parsed, so an enormous part is refused
// rather than read. Nothing is trimmed: a multipart field's value is the bytes
// between the headers and the boundary, so " 3600" is what a client sent and
// not something to guess past, and this parser is strict everywhere else too.
func parseExpiresIn(part *multipart.Part) (int64, error) {
	raw, err := io.ReadAll(io.LimitReader(part, maxExpiresInBytes+1))
	if err != nil {
		return 0, mapFileBodyErr(err)
	}
	if len(raw) > maxExpiresInBytes {
		return 0, errInvalid("expires_in_seconds must be an integer number of seconds")
	}
	secs, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return 0, errInvalid("expires_in_seconds must be an integer number of seconds")
	}
	if secs < minExpiresInSeconds || secs > maxExpiresInSeconds {
		return 0, errInvalid("expires_in_seconds must be between %d and %d",
			minExpiresInSeconds, maxExpiresInSeconds)
	}
	return secs, nil
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
// when it is specific, otherwise the filename extension through the pinned
// table (internal/mimetab — never mime.TypeByExtension, whose host-merged
// registry would let the serving host decide the stored value, #277),
// otherwise the generic octet-stream. The reference's exact derivation is
// unrecorded — an inference (docs/DIVERGENCES.md). The part Content-Type is a
// raw header value that can carry a non-UTF-8 byte; it is used only when
// storable, so a malformed one falls through to the extension rather than
// 500ing at the text-column bind (the #135 class, guarded on filename above
// and on scope_id/cursor in files.go).
func fileMimeType(part *multipart.Part, filename string) string {
	ct := part.Header.Get("Content-Type")
	if ct != "" && ct != "application/octet-stream" && storableText(ct) {
		return ct
	}
	return mimetab.ByPath(filename)
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
