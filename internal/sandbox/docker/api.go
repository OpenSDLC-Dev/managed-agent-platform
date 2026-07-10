package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// A hand-rolled slice of the Docker Engine API. The official client pulls in
// the whole moby module tree for the nine endpoints below; this is the same
// HTTP, over the same socket, in one file.
type apiClient struct {
	http *http.Client
	base string
}

// newAPIClient resolves the daemon address: the explicit host, else
// DOCKER_HOST, else the well-known socket.
func newAPIClient(host string) (*apiClient, error) {
	if host == "" {
		host = os.Getenv("DOCKER_HOST")
	}
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	switch {
	case strings.HasPrefix(host, "unix://"):
		path := strings.TrimPrefix(host, "unix://")
		tr := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", path)
			},
		}
		// No client timeout: exec streams for as long as the command runs.
		// Cancellation is the caller's context.
		return &apiClient{http: &http.Client{Transport: tr}, base: "http://docker"}, nil
	case strings.HasPrefix(host, "tcp://"):
		return &apiClient{http: &http.Client{}, base: "http://" + strings.TrimPrefix(host, "tcp://")}, nil
	}
	return nil, fmt.Errorf("docker: unsupported daemon address %q (want unix:// or tcp://)", host)
}

// apiError is a non-success reply. Status lets callers separate "no such
// container" (404) from "name taken" (409) without matching prose.
type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("docker: http %d: %s", e.Status, e.Message)
}

func statusIs(err error, code int) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Status == code
}

// containerGone identifies the daemon's "no such container" 404. The status
// alone cannot: the archive endpoints answer a missing path with "Could not
// find the file <path> in container <id>", and the exec endpoints answer a
// stale exec id with "No such exec instance". Neither means the sandbox died.
//
// The match is anchored, not a substring search, because that path is the
// agent's to choose: a file named "No such container" would otherwise make its
// own missing-file error read as a destroyed sandbox.
func containerGone(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Status == 404 && strings.HasPrefix(ae.Message, "No such container")
}

// request issues one call. The response is returned open on success (2xx and
// 304) so streaming endpoints can read it; the caller closes the body.
func (c *apiClient) request(ctx context.Context, method, path string, q url.Values, body io.Reader, contentType string) (*http.Response, error) {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, fmt.Errorf("docker: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker: %s %s: %w", method, path, err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		var decoded struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(msg, &decoded) == nil && decoded.Message != "" {
			return nil, &apiError{Status: resp.StatusCode, Message: decoded.Message}
		}
		return nil, &apiError{Status: resp.StatusCode, Message: strings.TrimSpace(string(msg))}
	}
	return resp, nil
}

// postJSON sends a JSON body and decodes the JSON reply into out.
func (c *apiClient) postJSON(ctx context.Context, path string, q url.Values, body, out any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("docker: encode request: %w", err)
	}
	resp, err := c.request(ctx, http.MethodPost, path, q, bytes.NewReader(encoded), "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("docker: decode %s reply: %w", path, err)
	}
	return nil
}

func (c *apiClient) getJSON(ctx context.Context, path string, out any) error {
	resp, err := c.request(ctx, http.MethodGet, path, nil, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("docker: decode %s reply: %w", path, err)
	}
	return nil
}

type containerInfo struct {
	ID    string `json:"Id"`
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
}

func (c *apiClient) inspectContainer(ctx context.Context, ref string) (containerInfo, error) {
	var info containerInfo
	err := c.getJSON(ctx, "/containers/"+ref+"/json", &info)
	return info, err
}

type hostConfig struct {
	NetworkMode string `json:"NetworkMode"`
	Init        bool   `json:"Init"`
}

type containerConfig struct {
	Image      string            `json:"Image"`
	Entrypoint []string          `json:"Entrypoint"`
	Cmd        []string          `json:"Cmd"`
	WorkingDir string            `json:"WorkingDir"`
	Labels     map[string]string `json:"Labels"`
	HostConfig hostConfig        `json:"HostConfig"`
}

func (c *apiClient) createContainer(ctx context.Context, name string, cfg containerConfig) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	err := c.postJSON(ctx, "/containers/create", url.Values{"name": {name}}, cfg, &out)
	return out.ID, err
}

// startContainer is idempotent: the daemon answers 304 when it is already up.
func (c *apiClient) startContainer(ctx context.Context, id string) error {
	resp, err := c.request(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil, "")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (c *apiClient) removeContainer(ctx context.Context, id string) error {
	resp, err := c.request(ctx, http.MethodDelete, "/containers/"+id,
		url.Values{"force": {"1"}, "v": {"1"}}, nil, "")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// pullImage streams the daemon's progress. Failures arrive as an {"error":…}
// object inside a 200 response, so the stream must be read, not discarded.
func (c *apiClient) pullImage(ctx context.Context, ref string) error {
	name, tag := splitImageRef(ref)
	q := url.Values{"fromImage": {name}}
	if tag != "" {
		q.Set("tag", tag)
	}
	resp, err := c.request(ctx, http.MethodPost, "/images/create", q, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	for {
		var msg struct {
			Error string `json:"error"`
		}
		if err := dec.Decode(&msg); err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("docker: pull %s: %w", ref, err)
		}
		if msg.Error != "" {
			return fmt.Errorf("docker: pull %s: %s", ref, msg.Error)
		}
	}
}

// splitImageRef separates the tag a pull needs. A digest reference carries its
// own identity and takes no tag; a colon inside the registry's host:port is
// not a tag.
func splitImageRef(ref string) (name, tag string) {
	if strings.Contains(ref, "@") {
		return ref, ""
	}
	i := strings.LastIndex(ref, ":")
	if i < 0 || strings.Contains(ref[i+1:], "/") {
		return ref, "latest"
	}
	return ref[:i], ref[i+1:]
}

type execConfig struct {
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	Cmd          []string `json:"Cmd"`
	WorkingDir   string   `json:"WorkingDir"`
}

func (c *apiClient) execCreate(ctx context.Context, id string, cfg execConfig) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	err := c.postJSON(ctx, "/containers/"+id+"/exec", nil, cfg, &out)
	return out.ID, err
}

// execStart returns the open multiplexed stream; the caller demuxes and closes.
func (c *apiClient) execStart(ctx context.Context, execID string) (io.ReadCloser, error) {
	body := bytes.NewReader([]byte(`{"Detach":false,"Tty":false}`))
	resp, err := c.request(ctx, http.MethodPost, "/exec/"+execID+"/start", nil, body, "application/json")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

type execInfo struct {
	Running  bool `json:"Running"`
	ExitCode int  `json:"ExitCode"`
}

func (c *apiClient) execInspect(ctx context.Context, execID string) (execInfo, error) {
	var info execInfo
	err := c.getJSON(ctx, "/exec/"+execID+"/json", &info)
	return info, err
}

func (c *apiClient) getArchive(ctx context.Context, id, path string) (io.ReadCloser, error) {
	resp, err := c.request(ctx, http.MethodGet, "/containers/"+id+"/archive",
		url.Values{"path": {path}}, nil, "")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (c *apiClient) putArchive(ctx context.Context, id, path string, tarball []byte) error {
	resp, err := c.request(ctx, http.MethodPut, "/containers/"+id+"/archive",
		url.Values{"path": {path}}, bytes.NewReader(tarball), "application/x-tar")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Docker's exec stream frame ids. Id 0 is stdin and never travels this way;
// id 3 carries a daemon-side error about the exec itself, not the command's
// output.
const (
	streamStdout    = 1
	streamStderr    = 2
	streamSystemErr = 3
)

// demux splits Docker's frame-multiplexed exec stream. Each frame is an 8-byte
// header — stream id, then a big-endian payload length — followed by payload.
// Output past limit is drained and dropped rather than buffered: the command
// must be free to finish, and the executor must not die of its output.
func demux(r io.Reader, limit int) (stdout, stderr []byte, truncated bool, err error) {
	keep := func(dst *[]byte, n int64) error {
		if room := int64(limit - len(*dst)); room > 0 {
			if room > n {
				room = n
			}
			buf := make([]byte, room)
			if _, err := io.ReadFull(r, buf); err != nil {
				return err
			}
			*dst = append(*dst, buf...)
			n -= room
		}
		if n > 0 {
			truncated = true
			if _, err := io.CopyN(io.Discard, r, n); err != nil {
				return err
			}
		}
		return nil
	}

	var header [8]byte
	for {
		if _, err := io.ReadFull(r, header[:]); err != nil {
			if err == io.EOF {
				return stdout, stderr, truncated, nil
			}
			return stdout, stderr, truncated, fmt.Errorf("docker: read exec frame: %w", err)
		}
		size := int64(binary.BigEndian.Uint32(header[4:]))

		var dst *[]byte
		switch header[0] {
		case streamStdout:
			dst = &stdout
		case streamStderr:
			dst = &stderr
		case streamSystemErr:
			// The daemon is reporting on the exec, not relaying the command.
			// Folding this into stdout would hand the model a plausible-looking
			// tool result built out of an infrastructure failure.
			var reason []byte
			if err := keepInto(r, &reason, size, 8<<10); err != nil {
				return stdout, stderr, truncated, fmt.Errorf("docker: read exec frame: %w", err)
			}
			return stdout, stderr, truncated, fmt.Errorf("docker: exec stream error: %s", reason)
		default:
			return stdout, stderr, truncated, fmt.Errorf("docker: unknown exec stream id %d", header[0])
		}
		if err := keep(dst, size); err != nil {
			return stdout, stderr, truncated, fmt.Errorf("docker: read exec frame: %w", err)
		}
	}
}

// keepInto reads n bytes, retaining at most limit of them.
func keepInto(r io.Reader, dst *[]byte, n int64, limit int64) error {
	if n > limit {
		n = limit
	}
	buf := make([]byte, n)
	_, err := io.ReadFull(r, buf)
	*dst = buf
	return err
}
