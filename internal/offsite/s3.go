package offsite

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// S3 is a minimal client for S3-compatible object storage: path-style
// requests signed with AWS Signature Version 4.
type S3 struct {
	Endpoint  string // https://<account>.r2.cloudflarestorage.com
	Region    string // "auto" for R2
	Bucket    string
	AccessKey string
	SecretKey string
	HTTP      *http.Client
	now       func() time.Time
}

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// partSize is the size of each part of a multipart upload; files up to one
// part go up in a single PUT.
var partSize int64 = 64 << 20

func (c *S3) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *S3) objectURL(key string, q url.Values) string {
	u := strings.TrimRight(c.Endpoint, "/") + "/" + c.Bucket
	if key != "" {
		u += "/" + escapePath(key)
	}
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// escapePath escapes each segment the way SigV4 canonical URIs need.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = uriEscape(s)
	}
	return strings.Join(segs, "/")
}

func uriEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func hmacSHA256(key []byte, s string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return m.Sum(nil)
}

// sign adds SigV4 headers to req. payloadHash is the hex SHA-256 of the
// body, or "UNSIGNED-PAYLOAD" for a streamed upload (allowed over HTTPS).
func (c *S3) sign(req *http.Request, payloadHash string) {
	now := time.Now().UTC()
	if c.now != nil {
		now = c.now().UTC()
	}
	amzDate := now.Format("20060102T150405Z")
	day := now.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	host := req.URL.Host
	if req.Host != "" {
		host = req.Host
	}

	names := []string{"host"}
	vals := map[string]string{"host": host}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if lk == "authorization" {
			continue
		}
		names = append(names, lk)
		vals[lk] = strings.TrimSpace(strings.Join(v, ","))
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, n := range names {
		canonHeaders.WriteString(n + ":" + vals[n] + "\n")
	}
	signed := strings.Join(names, ";")

	// Canonical query: keys and values escaped, sorted by key.
	q := req.URL.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var cq []string
	for _, k := range keys {
		vs := append([]string{}, q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			cq = append(cq, uriEscape(k)+"="+uriEscape(v))
		}
	}
	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	canon := strings.Join([]string{req.Method, path, strings.Join(cq, "&"), canonHeaders.String(), signed, payloadHash}, "\n")
	sum := sha256.Sum256([]byte(canon))
	scope := day + "/" + c.Region + "/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	k := hmacSHA256([]byte("AWS4"+c.SecretKey), day)
	k = hmacSHA256(k, c.Region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", c.AccessKey, scope, signed, sig))
}

// S3Error is an error answer from the storage service.
type S3Error struct {
	Status  int
	Code    string
	Message string
}

func (e *S3Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("storage answered %d %s: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("storage answered %d", e.Status)
}

func (c *S3) do(ctx context.Context, method, key string, q url.Values, body io.Reader, size int64, payloadHash string, out any) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.objectURL(key, q), body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
	}
	c.sign(req, payloadHash)
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var e struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		}
		xml.Unmarshal(b, &e)
		return nil, &S3Error{Status: resp.StatusCode, Code: e.Code, Message: e.Message}
	}
	if out != nil {
		defer resp.Body.Close()
		return resp, xml.NewDecoder(resp.Body).Decode(out)
	}
	return resp, nil
}

func hashOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// PutFile uploads path as key, in parts when it's bigger than one part.
func (c *S3) PutFile(ctx context.Context, key, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.Size() <= partSize {
		resp, err := c.do(ctx, "PUT", key, nil, f, fi.Size(), "UNSIGNED-PAYLOAD", nil)
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	}
	var start struct {
		UploadID string `xml:"UploadId"`
	}
	if _, err := c.do(ctx, "POST", key, url.Values{"uploads": {""}}, nil, 0, emptySHA256, &start); err != nil {
		return err
	}
	abort := func() {
		resp, err := c.do(context.Background(), "DELETE", key, url.Values{"uploadId": {start.UploadID}}, nil, 0, emptySHA256, nil)
		if err == nil {
			resp.Body.Close()
		}
	}
	type part struct {
		Number int    `xml:"PartNumber"`
		ETag   string `xml:"ETag"`
	}
	var parts []part
	for n, off := 1, int64(0); off < fi.Size(); n, off = n+1, off+partSize {
		size := min(partSize, fi.Size()-off)
		var resp *http.Response
		var err error
		for try := 0; try < 3; try++ { // a part is worth retrying, not the whole file
			resp, err = c.do(ctx, "PUT", key, url.Values{"partNumber": {strconv.Itoa(n)}, "uploadId": {start.UploadID}}, io.NewSectionReader(f, off, size), size, "UNSIGNED-PAYLOAD", nil)
			if err == nil || ctx.Err() != nil {
				break
			}
		}
		if err != nil {
			abort()
			return fmt.Errorf("part %d: %w", n, err)
		}
		resp.Body.Close()
		parts = append(parts, part{n, resp.Header.Get("ETag")})
	}
	body, _ := xml.Marshal(struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Parts   []part   `xml:"Part"`
	}{Parts: parts})
	resp, err := c.do(ctx, "POST", key, url.Values{"uploadId": {start.UploadID}}, bytes.NewReader(body), int64(len(body)), hashOf(body), nil)
	if err != nil {
		abort()
		return err
	}
	resp.Body.Close()
	return nil
}

// Get returns key's contents; the caller closes it.
func (c *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := c.do(ctx, "GET", key, nil, nil, 0, emptySHA256, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (c *S3) Delete(ctx context.Context, key string) error {
	resp, err := c.do(ctx, "DELETE", key, nil, nil, 0, emptySHA256, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Object is a stored object.
type Object struct {
	Key      string    `xml:"Key" json:"key"`
	Size     int64     `xml:"Size" json:"size"`
	Modified time.Time `xml:"LastModified" json:"modified"`
}

// List returns the objects under prefix.
func (c *S3) List(ctx context.Context, prefix string) ([]Object, error) {
	var all []Object
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		var res struct {
			Contents  []Object `xml:"Contents"`
			Truncated bool     `xml:"IsTruncated"`
			Next      string   `xml:"NextContinuationToken"`
		}
		if _, err := c.do(ctx, "GET", "", q, nil, 0, emptySHA256, &res); err != nil {
			return nil, err
		}
		all = append(all, res.Contents...)
		if !res.Truncated || res.Next == "" {
			break
		}
		token = res.Next
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Key < all[j].Key })
	return all, nil
}
