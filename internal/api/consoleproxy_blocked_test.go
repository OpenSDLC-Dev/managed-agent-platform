package api_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/blobtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
)

type waitingDownloadStore struct {
	blob.Store
	entered, release chan struct{}
}

func (s *waitingDownloadStore) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	close(s.entered)
	select {
	case <-s.release:
		return s.Store.Get(ctx, key)
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}
}

// Finishing a normal response cancels its pending recheck, not its buffered
// HTTP answer. A parent-cancelled lookup must not expire the socket deadline.
func TestPrivateProxyCompletionStopsPendingLookup(t *testing.T) {
	pool := newPoolWithKey(t)
	blobs := blobtest.Mem()
	store := &waitingDownloadStore{Store: blobs, entered: make(chan struct{}), release: make(chan struct{})}
	manager := login.New(pool, login.Config{Mode: login.ModePassword, PublicOrigin: nativeOrigin, Development: true})
	srv := httptest.NewServer(api.NewHandler(pool, store, nil, nil, api.WithLogin(manager, nil)))
	t.Cleanup(srv.Close)
	s := &nativeServer{tserver: &tserver{t: t, url: srv.URL, pool: pool, blobs: blobs}, manager: manager}
	caller := seedMember(t, s, "finishing-reader", "developer", "console")
	cookie, _ := s.issue(caller)
	id := domain.NewID(domain.PrefixFile).String()
	const content = "complete healthy binary response"
	if err := blobs.Put(context.Background(), "files/"+id, bytes.NewBufferString(content), int64(len(content)), "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO files(id,filename,mime_type,size_bytes,downloadable) VALUES ($1,'small.bin','application/octet-stream',$2,true)`, id, len(content)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := managementAsync(ctx, s, "GET", privateProxy+"/files/"+id+"/content", "", nativeHeaders(cookie, ""))
	select {
	case <-store.entered:
	case <-ctx.Done():
		t.Fatal("download never entered object storage")
	}
	blocker := blockPrivateSessions(t, s, ctx)
	observePrivateSQL(t, s, ctx, 1, false)
	close(store.release)
	select {
	case reply := <-done:
		if reply.err != nil || reply.status != 200 || string(reply.raw) != content {
			t.Fatalf("normal completion was mistaken for withdrawal: status=%d body=%q err=%v", reply.status, reply.raw, reply.err)
		}
	case <-ctx.Done():
		t.Fatal("normal response waited for the blocked recheck")
	}
	observePrivateSQL(t, s, ctx, 0, false)
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
}

// Context cancellation alone does not interrupt a blocked socket Write. A real
// non-reading client and an observed SQL wait must still withdraw within 15s.
func TestPrivateProxyBlockedClientStillWithdraws(t *testing.T) {
	pool := newPoolWithKey(t)
	blobs := blobtest.Mem()
	manager := login.New(pool, login.Config{Mode: login.ModePassword, PublicOrigin: nativeOrigin, Development: true})
	ended := make(chan http.ConnState, 2)
	srv := httptest.NewUnstartedServer(api.NewHandler(pool, blobs, nil, nil, api.WithLogin(manager, nil)))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateIdle || state == http.StateClosed {
			ended <- state
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	s := &nativeServer{tserver: &tserver{t: t, url: srv.URL, pool: pool, blobs: blobs}, manager: manager}
	caller := seedMember(t, s, "blocked-reader", "developer", "console")
	cookie, _ := s.issue(caller)
	id := domain.NewID(domain.PrefixFile).String()
	const size = 64 << 20 // well beyond both TCP buffers; never read by the client until withdrawal
	if err := blobs.Put(context.Background(), "files/"+id, bytes.NewReader(make([]byte, size)), size, "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO files(id,filename,mime_type,size_bytes,downloadable) VALUES ($1,'large.bin','application/octet-stream',$2,true)`, id, size); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.(*net.TCPConn).SetReadBuffer(1024); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	path := privateProxy + "/files/" + id + "/content"
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nCookie: %s\r\n\r\n", path, u.Host, cookie); err != nil {
		t.Fatal(err)
	}
	res, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "GET"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(); res.Body.Close() })
	if res.StatusCode != 200 || res.ContentLength != size || res.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("download did not start: status=%d headers=%v", res.StatusCode, res.Header)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	started := time.Now()
	blocker := blockPrivateSessions(t, s, ctx)
	observePrivateSQL(t, s, ctx, 1, false)
	select {
	case state := <-ended:
		t.Fatalf("large response already finished before lookup timeout: %s", state)
	default:
	}
	select {
	case state := <-ended:
		if state != http.StateClosed {
			t.Fatalf("withdrawn download left a reusable connection: %s", state)
		}
	case <-ctx.Done():
		t.Fatal("blocked socket outlived the 15s withdrawal budget")
	}
	t.Logf("blocked socket withdrew in %s", time.Since(started))
	observePrivateSQL(t, s, ctx, 0, false)
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// Observe the actual truncated response after the server has closed it.
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, res.Body)
	if err == nil || n >= size {
		t.Fatalf("withdrawn response was not truncated: bytes=%d err=%v", n, err)
	}
}
