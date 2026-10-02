package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

// fakeS3 is an in-memory, path-style S3 for one bucket, paging ListObjectsV2 two keys at a time.
// It checks that every request is signed, and that a signed payload hash matches the body.
type fakeS3 struct {
	mu      sync.Mutex
	t       *testing.T
	bucket  string
	objects map[string][]byte
	created bool
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AK/") || r.Header.Get("x-amz-date") == "" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == f.bucket && r.Method == http.MethodPut {
		if f.created {
			w.WriteHeader(http.StatusConflict)
		}
		f.created = true
		return
	}
	if !strings.HasPrefix(path, f.bucket+"/") && path != f.bucket+"/" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	key := strings.TrimPrefix(path, f.bucket+"/")
	switch r.Method {
	case http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		if h := r.Header.Get("x-amz-content-sha256"); h != "UNSIGNED-PAYLOAD" {
			sum := sha256.Sum256(b)
			if h != hex.EncodeToString(sum[:]) {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, "XAmzContentSHA256Mismatch")
				return
			}
		}
		f.objects[key] = b
	case http.MethodGet, http.MethodHead:
		if r.URL.Query().Get("list-type") == "2" {
			f.list(w, r)
			return
		}
		b, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("ETag", `"etag-`+key+`"`)
		w.Header().Set("Content-Length", fmt.Sprint(len(b)))
		w.Header().Set("Last-Modified", "Wed, 01 Oct 2026 12:00:00 GMT")
		if r.Method == http.MethodGet {
			_, _ = w.Write(b)
		}
	case http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (f *fakeS3) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	start := 0
	if tok := r.URL.Query().Get("continuation-token"); tok != "" {
		fmt.Sscan(tok, &start)
	}
	end := start + 2
	if end > len(keys) {
		end = len(keys)
	}
	var b strings.Builder
	b.WriteString("<ListBucketResult>")
	for _, k := range keys[start:end] {
		fmt.Fprintf(&b, "<Contents><Key>%s</Key><Size>%d</Size><ETag>\"e\"</ETag><LastModified>2026-10-01T12:00:00Z</LastModified></Contents>", k, len(f.objects[k]))
	}
	if end < len(keys) {
		fmt.Fprintf(&b, "<IsTruncated>true</IsTruncated><NextContinuationToken>%d</NextContinuationToken>", end)
	}
	b.WriteString("</ListBucketResult>")
	_, _ = io.WriteString(w, b.String())
}

func newTestClient(t *testing.T) (*fakeS3, *Client) {
	t.Helper()
	f := &fakeS3{t: t, bucket: "backups", objects: map[string][]byte{}}
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	c, err := New(Config{Endpoint: ts.URL, Region: "nyc3", Bucket: "backups", AccessKey: "AK", SecretKey: "SK", PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

func TestObjectRoundTrip(t *testing.T) {
	_, c := newTestClient(t)
	ctx := context.Background()
	if err := c.CreateBucket(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateBucket(ctx); err != nil {
		t.Fatalf("an existing bucket is not an error: %v", err)
	}

	body := []byte("volume-backup-bytes")
	sum := sha256.Sum256(body)
	if err := c.PutObject(ctx, "incus/vol/a.tar.gz", bytes.NewReader(body), int64(len(body)), hex.EncodeToString(sum[:]), "application/gzip"); err != nil {
		t.Fatal(err)
	}
	// A wrong hash is refused by the store, and the error says so.
	if err := c.PutObject(ctx, "incus/vol/bad", bytes.NewReader(body), int64(len(body)), strings.Repeat("0", 64), ""); err == nil || !strings.Contains(err.Error(), "Mismatch") {
		t.Fatalf("a payload that does not match its signed hash must fail: %v", err)
	}

	head, err := c.HeadObject(ctx, "incus/vol/a.tar.gz")
	if err != nil || head.Size != int64(len(body)) || head.ETag != "etag-incus/vol/a.tar.gz" || head.LastModified.IsZero() {
		t.Fatalf("head: %+v %v", head, err)
	}
	rc, n, err := c.GetObject(ctx, "incus/vol/a.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if n != int64(len(body)) || !bytes.Equal(got, body) {
		t.Fatalf("get: %q (%d)", got, n)
	}

	if _, _, err := c.GetObject(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a missing object must be ErrNotFound: %v", err)
	}
	if _, err := c.HeadObject(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("head of a missing object must be ErrNotFound: %v", err)
	}
	if err := c.DeleteObject(ctx, "incus/vol/a.tar.gz"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteObject(ctx, "incus/vol/a.tar.gz"); err != nil {
		t.Fatalf("deleting a missing object is not an error: %v", err)
	}
}

func TestListObjectsFollowsContinuationTokens(t *testing.T) {
	f, c := newTestClient(t)
	for _, k := range []string{"incus/a/1", "incus/a/2", "incus/a/3", "incus/a/4", "incus/a/5", "other/x"} {
		f.objects[k] = []byte(k)
	}
	objs, err := c.ListObjects(context.Background(), "incus/a/")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 5 || objs[4].Key != "incus/a/5" || objs[0].LastModified.IsZero() {
		t.Fatalf("every page must be read, and only under the prefix: %+v", objs)
	}
}

func TestNewValidatesTheConfig(t *testing.T) {
	for _, cfg := range []Config{
		{Region: "r", Bucket: "b"},
		{Endpoint: "https://x", Region: "r"},
		{Endpoint: "https://x", Bucket: "b"},
		{Endpoint: "not a url", Region: "r", Bucket: "b"},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("config %+v must be refused", cfg)
		}
	}
	c, err := New(Config{Endpoint: "https://nyc3.example.com", Region: "nyc3", Bucket: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if u := c.objectURL("k/1"); u.Host != "b.nyc3.example.com" || u.Path != "/k/1" {
		t.Fatalf("virtual-hosted URL: %s", u)
	}
}
