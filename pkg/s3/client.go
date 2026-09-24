package s3

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrNotFound is returned when an object does not exist.
var ErrNotFound = errors.New("s3: object not found")

// Config describes an S3-compatible endpoint (Spaces, MinIO, AWS S3, ...).
type Config struct {
	Endpoint  string // e.g. https://nyc3.digitaloceanspaces.com
	Region    string // e.g. nyc3
	Bucket    string
	AccessKey string
	SecretKey string
	PathStyle bool // bucket in the path (true for Spaces/MinIO)
}

// Object is a stored object returned by list/head.
type Object struct {
	Key          string
	Size         int64
	ETag         string
	LastModified time.Time
}

// Client is a minimal S3 client using only the standard library.
type Client struct {
	cfg        Config
	base       *url.URL
	httpClient *http.Client
}

// New validates cfg and returns a client.
func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("s3: endpoint is required")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("s3: bucket is required")
	}
	if cfg.Region == "" {
		return nil, errors.New("s3: region is required")
	}
	base, err := url.Parse(strings.TrimRight(cfg.Endpoint, "/"))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("s3: invalid endpoint %q", cfg.Endpoint)
	}
	return &Client{cfg: cfg, base: base, httpClient: &http.Client{Timeout: 30 * time.Minute}}, nil
}

// Key builds a full object URL, honouring path-style addressing.
func (c *Client) objectURL(key string) *url.URL {
	u := *c.base
	key = strings.TrimPrefix(key, "/")
	if c.cfg.PathStyle {
		u.Path = "/" + c.cfg.Bucket + "/" + key
	} else {
		u.Host = c.cfg.Bucket + "." + c.base.Host
		u.Path = "/" + key
	}
	return &u
}

func (c *Client) hostFor() string {
	if c.cfg.PathStyle {
		return c.base.Host
	}
	return c.cfg.Bucket + "." + c.base.Host
}

func (c *Client) do(req *http.Request, payloadHash string, extraSigned map[string]string) (*http.Response, error) {
	req.Host = c.hostFor()
	sign(req, c.cfg.AccessKey, c.cfg.SecretKey, c.cfg.Region, payloadHash, time.Now(), extraSigned)
	return c.httpClient.Do(req)
}

// PutObject uploads body (of the given size) to key. sha256hex must be the
// lowercase hex SHA-256 of the content; pass "" to use UNSIGNED-PAYLOAD.
func (c *Client) PutObject(ctx context.Context, key string, body io.Reader, size int64, sha256hex, contentType string) error {
	if sha256hex == "" {
		sha256hex = unsignedValue
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.objectURL(key).String(), body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	extra := map[string]string{}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
		extra["content-type"] = contentType
	}
	resp, err := c.do(req, sha256hex, extra)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusError("put object", resp)
	}
	return nil
}

// GetObject downloads key. The caller must close the returned reader.
func (c *Client) GetObject(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.objectURL(key).String(), nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := c.do(req, emptySHA256, nil)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, 0, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, 0, statusError("get object", resp)
	}
	return resp.Body, resp.ContentLength, nil
}

// HeadObject returns metadata for key.
func (c *Client) HeadObject(ctx context.Context, key string) (*Object, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.objectURL(key).String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req, emptySHA256, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusError("head object", resp)
	}
	o := &Object{Key: key, Size: resp.ContentLength, ETag: strings.Trim(resp.Header.Get("ETag"), `"`)}
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		o.LastModified, _ = http.ParseTime(lm)
	}
	return o, nil
}

// DeleteObject removes key. A missing object is not an error.
func (c *Client) DeleteObject(ctx context.Context, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.objectURL(key).String(), nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req, emptySHA256, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return statusError("delete object", resp)
	}
	return nil
}

type listResult struct {
	Contents []struct {
		Key          string `xml:"Key"`
		Size         int64  `xml:"Size"`
		ETag         string `xml:"ETag"`
		LastModified string `xml:"LastModified"`
	} `xml:"Contents"`
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
}

// ListObjects returns every object under prefix (handles pagination).
func (c *Client) ListObjects(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	token := ""
	for {
		q := url.Values{}
		q.Set("list-type", "2")
		if prefix != "" {
			q.Set("prefix", prefix)
		}
		if token != "" {
			q.Set("continuation-token", token)
		}
		u := c.objectURL("")
		u.RawQuery = canonicalQuery(q)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		resp, err := c.do(req, emptySHA256, nil)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("list objects: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		var res listResult
		if err := xml.Unmarshal(body, &res); err != nil {
			return nil, fmt.Errorf("list objects: parse response: %w", err)
		}
		for _, o := range res.Contents {
			obj := Object{Key: o.Key, Size: o.Size, ETag: strings.Trim(o.ETag, `"`)}
			if t, err := time.Parse(time.RFC3339, o.LastModified); err == nil {
				obj.LastModified = t
			}
			out = append(out, obj)
		}
		if !res.IsTruncated || res.NextContinuationToken == "" {
			return out, nil
		}
		token = res.NextContinuationToken
	}
}

// statusError reads a short body and returns an error describing the status.
func statusError(op string, resp *http.Response) error {
	buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(buf))
	if msg == "" {
		msg = resp.Status
	}
	return fmt.Errorf("%s: status %d: %s", op, resp.StatusCode, msg)
}
