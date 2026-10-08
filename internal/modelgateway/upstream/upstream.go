// Package upstream is the model gateway's side of a call to a provider: the
// HTTP client every attempt rides, and a reader that takes a server-sent
// event stream apart event by event so the relay can pass each on as it
// arrives (docs/plan/59_model-gateway.md, "Two request paths").
package upstream

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

// NewClient returns the client every attempt uses. It follows no redirect: a
// 3xx comes back as the response, which the router counts as a failed
// attempt, so a credential header reaches only the host an administrator
// configured — as the web backends' clients do (internal/webtool/tavily).
// Provider hosts are admin-configured, so the client dials them directly
// rather than through internal/dialguard: admin rights are the vouching.
//
// It sets no overall timeout. A streamed answer legitimately runs for
// minutes; silence is bounded by provider.StallGuard instead.
func NewClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = 15 * time.Second
	t.MaxIdleConnsPerHost = 32
	return &http.Client{
		Transport:     t,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// MaxEvent bounds one event, so an upstream that never sends a blank line
// cannot grow the gateway's memory without end.
const MaxEvent = 16 << 20

// ErrEventTooLarge reports an event longer than MaxEvent.
var ErrEventTooLarge = errors.New("upstream sent an event larger than 16 MiB")

// Event is one block of a server-sent event stream: the lines up to and
// including the blank line that ends it. A block of comment lines alone — a
// keep-alive — is an event with no Name and no Data.
type Event struct {
	Raw  []byte // the block as received
	Name string // its event field
	Data []byte // its data lines, joined by "\n"; nil when it has none
}

// WithData returns the block with its data replaced by data, every other
// line kept in place: the data lines collapse into one, where the first was.
func (e Event) WithData(data []byte) []byte {
	var out bytes.Buffer
	written := false
	for _, line := range bytes.SplitAfter(e.Raw, []byte("\n")) {
		if field(line) != "data" {
			out.Write(line)
			continue
		}
		if !written {
			out.WriteString("data: ")
			out.Write(data)
			out.WriteByte('\n')
			written = true
		}
	}
	return out.Bytes()
}

// Reader reads a stream's events.
type Reader struct{ br *bufio.Reader }

// NewReader reads events from r.
func NewReader(r io.Reader) *Reader {
	if br, ok := r.(*bufio.Reader); ok {
		return &Reader{br: br}
	}
	return &Reader{br: bufio.NewReader(r)}
}

// Next returns the next event. At the end of the stream it returns io.EOF,
// with whatever trailing lines had no blank line after them as a last event.
func (r *Reader) Next() (Event, error) {
	var e Event
	var data [][]byte
	for {
		line, err := r.br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			// A long line: collect it whole, within the event's bound. The
			// slice points into the reader's buffer, which the next read
			// overwrites, so it is copied first.
			head := append([]byte(nil), line...)
			rest, rerr := r.longLine(len(e.Raw) + len(head))
			line, err = append(head, rest...), rerr
		}
		if len(e.Raw)+len(line) > MaxEvent {
			return Event{}, ErrEventTooLarge
		}
		e.Raw = append(e.Raw, line...)
		trimmed := bytes.TrimRight(line, "\r\n")
		switch f := field(line); {
		case len(line) > 0 && len(trimmed) == 0:
			// The blank line that ends the event.
			e.Data = joined(data)
			return e, nil
		case f == "event":
			e.Name = string(value(trimmed))
		case f == "data":
			data = append(data, append([]byte(nil), value(trimmed)...))
		}
		if err != nil {
			e.Data = joined(data)
			return e, err
		}
	}
}

// joined is an event's data lines joined by "\n": nil when it has none, and
// empty but not nil when its one data line is empty, which is data still —
// a client dispatches it.
func joined(data [][]byte) []byte {
	if data == nil {
		return nil
	}
	return append([]byte{}, bytes.Join(data, []byte("\n"))...)
}

func (r *Reader) longLine(have int) ([]byte, error) {
	var out []byte
	for {
		chunk, err := r.br.ReadSlice('\n')
		if have+len(out)+len(chunk) > MaxEvent {
			return nil, ErrEventTooLarge
		}
		out = append(out, chunk...)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return out, err
		}
	}
}

// field names a line's field: the text before its first colon, "" for a
// comment, which starts with one.
func field(line []byte) string {
	i := bytes.IndexByte(line, ':')
	if i < 0 {
		return string(bytes.TrimRight(line, "\r\n"))
	}
	return string(line[:i])
}

// value is a field line's value: after the colon and one optional space.
func value(line []byte) []byte {
	i := bytes.IndexByte(line, ':')
	if i < 0 {
		return nil
	}
	v := line[i+1:]
	if len(v) > 0 && v[0] == ' ' {
		v = v[1:]
	}
	return v
}
