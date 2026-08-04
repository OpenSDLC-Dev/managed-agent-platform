// Package mimetab is the platform's pinned extension → MIME table — the one
// source both writers of the files registry consult, so the same filename
// yields the same registry row wherever it enters: the outputs harvest
// (internal/executor, #264) and the upload endpoint's extension fallback
// (internal/api, #277). Neither may consult mime.TypeByExtension: it merges
// the host's /etc/mime.types (or registry), so a host-dependent lookup would
// let the serving host decide a wire-visible value.
package mimetab

import (
	"path"
	"strings"
)

// byExt is the pinned table. It is a snapshot of Go 1.26's builtin table
// (mime.builtinTypesLower — copied, not referenced, so a toolchain upgrade
// cannot change registry rows either) plus a bounded set of textual
// deliverable types that table lacks. The grader's inline rule (text/*,
// application/json — inlineableMime, internal/brain/grader.go) reads these
// values, so the textual additions carry text/* deliberately (.tar, the one
// binary addition, keeps application/x-tar): an entry's value decides whether
// that deliverable's content reaches the grader. Unlisted extensions fall
// back to application/octet-stream on every host; extend the table when a
// real deliverable class needs it.
var byExt = map[string]string{
	// Go 1.26 mime.builtinTypesLower, verbatim:
	".ai":    "application/postscript",
	".apk":   "application/vnd.android.package-archive",
	".apng":  "image/apng",
	".avif":  "image/avif",
	".bin":   "application/octet-stream",
	".bmp":   "image/bmp",
	".com":   "application/octet-stream",
	".css":   "text/css; charset=utf-8",
	".csv":   "text/csv; charset=utf-8",
	".doc":   "application/msword",
	".docx":  "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".ehtml": "text/html; charset=utf-8",
	".eml":   "message/rfc822",
	".eps":   "application/postscript",
	".exe":   "application/octet-stream",
	".flac":  "audio/flac",
	".gif":   "image/gif",
	".gz":    "application/gzip",
	".htm":   "text/html; charset=utf-8",
	".html":  "text/html; charset=utf-8",
	".ico":   "image/vnd.microsoft.icon",
	".ics":   "text/calendar; charset=utf-8",
	".jfif":  "image/jpeg",
	".jpeg":  "image/jpeg",
	".jpg":   "image/jpeg",
	".js":    "text/javascript; charset=utf-8",
	".json":  "application/json",
	".m4a":   "audio/mp4",
	".mjs":   "text/javascript; charset=utf-8",
	".mp3":   "audio/mpeg",
	".mp4":   "video/mp4",
	".oga":   "audio/ogg",
	".ogg":   "audio/ogg",
	".ogv":   "video/ogg",
	".opus":  "audio/ogg",
	".pdf":   "application/pdf",
	".pjp":   "image/jpeg",
	".pjpeg": "image/jpeg",
	".png":   "image/png",
	".ppt":   "application/vnd.ms-powerpoint",
	".pptx":  "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".ps":    "application/postscript",
	".rdf":   "application/rdf+xml",
	".rtf":   "application/rtf",
	".shtml": "text/html; charset=utf-8",
	".svg":   "image/svg+xml",
	".text":  "text/plain; charset=utf-8",
	".tif":   "image/tiff",
	".tiff":  "image/tiff",
	".txt":   "text/plain; charset=utf-8",
	".vtt":   "text/vtt; charset=utf-8",
	".wasm":  "application/wasm",
	".wav":   "audio/wav",
	".webm":  "audio/webm",
	".webp":  "image/webp",
	".xbl":   "text/xml; charset=utf-8",
	".xbm":   "image/x-xbitmap",
	".xht":   "application/xhtml+xml",
	".xhtml": "application/xhtml+xml",
	".xls":   "application/vnd.ms-excel",
	".xlsx":  "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".xml":   "text/xml; charset=utf-8",
	".xsl":   "text/xml; charset=utf-8",
	".zip":   "application/zip",
	// Textual deliverable types the builtin table lacks — text/* so the
	// grader inlines them (see the note above), plus .tar for symmetry with
	// the builtin .gz/.zip:
	".ini":   "text/plain; charset=utf-8",
	".jsonl": "text/plain; charset=utf-8",
	".log":   "text/plain; charset=utf-8",
	".md":    "text/markdown; charset=utf-8",
	".py":    "text/x-python; charset=utf-8",
	".rst":   "text/x-rst; charset=utf-8",
	".sql":   "text/x-sql; charset=utf-8",
	".tar":   "application/x-tar",
	".tex":   "text/x-tex; charset=utf-8",
	".toml":  "text/x-toml; charset=utf-8",
	".tsv":   "text/tab-separated-values; charset=utf-8",
	".yaml":  "text/yaml; charset=utf-8",
	".yml":   "text/yaml; charset=utf-8",
}

// ByPath returns the pinned MIME type for the path's extension, case-folded,
// or application/octet-stream when the extension is unlisted. path.Ext (not
// filepath.Ext) on purpose: every caller hands it a slash-separated sandbox
// path or a validated bare filename, never a host-native path.
func ByPath(p string) string {
	if m := byExt[strings.ToLower(path.Ext(p))]; m != "" {
		return m
	}
	return "application/octet-stream"
}
