package anyshare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/unofficialpku/disk/internal/diagnostic"
)

type TokenSource interface {
	AccessToken() string
	Refresh(context.Context, string) error
}

type Client struct {
	Trace    *diagnostic.Logger
	base     string
	tokens   TokenSource
	api      *http.Client
	objects  *http.Client
	PartSize int64
	PageSize int
}

// SetTrace is configured before the client is used by a mount.
func (c *Client) SetTrace(trace *diagnostic.Logger) {
	c.Trace = trace
	c.api.Transport = trace.Transport(c.api.Transport, "API")
	c.objects.Transport = trace.Transport(c.objects.Transport, "object")
}

func New(base string, tokens TokenSource) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("invalid server URL")
	}
	if tokens == nil {
		return nil, errors.New("missing authentication")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 60 * time.Second
	tr.MaxIdleConnsPerHost = 16
	noRedirect := func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{base: u.String(), tokens: tokens, api: &http.Client{Transport: tr, Timeout: 90 * time.Second, CheckRedirect: noRedirect}, objects: &http.Client{Transport: tr, CheckRedirect: noRedirect}, PartSize: 16 << 20, PageSize: 1000}, nil
}

func (c *Client) request(ctx context.Context, method, path string, body any) ([]byte, error) {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		c.Trace.Printf(ctx, "API %s %q: acquiring access token", method, strings.SplitN(path, "?", 2)[0])
		token := c.tokens.AccessToken()
		req, err := http.NewRequestWithContext(ctx, method, c.base+"/api"+path, bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		resp, err := c.api.Do(req)
		if err != nil {
			return nil, safeTransportError(err)
		}
		b, readErr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if resp.StatusCode == 401 && attempt == 0 {
			c.Trace.Printf(ctx, "HTTP 401: refreshing session before one retry")
			if err := c.tokens.Refresh(ctx, token); err != nil {
				return nil, fmt.Errorf("refresh session: %w", err)
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 || resp.StatusCode == 202 {
			return nil, responseError(resp.StatusCode, b)
		}
		return b, nil
	}
	return nil, ErrPermission
}

// Signed URLs carry credentials; never include them in returned transport errors.
func safeTransportError(err error) error {
	var u *url.Error
	if errors.As(err, &u) {
		return fmt.Errorf("HTTP transport: %w", u.Err)
	}
	return err
}

func (c *Client) post(ctx context.Context, path string, body any, out any) error {
	b, err := c.request(ctx, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (c *Client) Roots(ctx context.Context) ([]Entry, error) {
	b, err := c.request(ctx, http.MethodGet, "/efast/v1/entry-doc-lib", nil)
	if err != nil {
		return nil, err
	}
	var entries []Entry
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, err
	}
	for i := range entries {
		entries[i].Dir = true
		entries[i].Size = -1
	}
	// Disambiguate duplicate library names deterministically.
	counts := map[string]int{}
	for _, e := range entries {
		counts[e.Name]++
	}
	for i := range entries {
		if !ValidName(entries[i].Name) {
			return nil, fmt.Errorf("library has an unsupported filename")
		}
		if counts[entries[i].Name] > 1 {
			entries[i].Name += " [" + strings.TrimPrefix(entries[i].ID, "gns://") + "]"
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

func (c *Client) List(ctx context.Context, id string) ([]Entry, error) {
	var result []Entry
	marker := ""
	seen := map[string]bool{}
	for {
		q := url.Values{"limit": {fmt.Sprint(c.PageSize)}, "sort": {"name"}, "direction": {"asc"}}
		if marker != "" {
			q.Set("marker", marker)
		}
		b, err := c.request(ctx, http.MethodGet, "/efast/v1/folders/"+url.PathEscape(id)+"/sub_objects?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		var page struct {
			Dirs  []Entry `json:"dirs"`
			Files []Entry `json:"files"`
			Next  string  `json:"next_marker"`
		}
		if err := json.Unmarshal(b, &page); err != nil {
			return nil, err
		}
		for i := range page.Dirs {
			page.Dirs[i].Dir = true
			page.Dirs[i].Size = -1
		}
		result = append(result, page.Dirs...)
		result = append(result, page.Files...)
		if page.Next == "" {
			break
		}
		if seen[page.Next] {
			return nil, errors.New("server repeated a directory pagination marker")
		}
		seen[page.Next] = true
		marker = page.Next
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func (c *Client) Stat(ctx context.Context, entry Entry) (Entry, error) {
	kind := "file"
	if entry.Dir {
		kind = "dir"
	}
	var out Entry
	err := c.post(ctx, "/efast/v1/"+kind+"/attribute", map[string]any{"docid": entry.ID}, &out)
	if err != nil {
		return Entry{}, err
	}
	out.Dir = entry.Dir
	if out.ID == "" {
		out.ID = entry.ID
	}
	if out.Name == "" || ParentID(entry.ID) == "" {
		out.Name = entry.Name
	}
	return out, nil
}

func (c *Client) Mkdir(ctx context.Context, parent, name string) (Entry, error) {
	if !ValidName(name) {
		return Entry{}, errors.New("invalid filename")
	}
	var out Entry
	if err := c.post(ctx, "/efast/v1/dir/create", map[string]any{"docid": parent, "name": name, "ondup": 1}, &out); err != nil {
		return Entry{}, err
	}
	out.Dir, out.Size, out.Name = true, -1, name
	return out, nil
}

func (c *Client) Delete(ctx context.Context, e Entry) error {
	if err := ValidateDelete(e); err != nil {
		return err
	}
	kind := "file"
	if e.Dir {
		kind = "dir"
	}
	return c.post(ctx, "/efast/v1/"+kind+"/delete", map[string]any{"docid": e.ID, "check_upload_process": true}, nil)
}

func (c *Client) Can(ctx context.Context, id, permission string) (bool, error) {
	allow, err := c.permissions(ctx, id)
	if err != nil {
		return false, err
	}
	for _, p := range allow {
		if p == permission {
			return true, nil
		}
	}
	return false, nil
}

func (c *Client) Writable(ctx context.Context, id string) (bool, error) {
	allow, err := c.permissions(ctx, id)
	if err != nil {
		return false, err
	}
	for _, permission := range allow {
		if permission == "create" || permission == "modify" {
			return true, nil
		}
	}
	return false, nil
}

func (c *Client) permissions(ctx context.Context, id string) ([]string, error) {
	var result struct {
		Allow []string `json:"allow"`
	}
	if err := c.post(ctx, "/eacp/v1/perm1/checkall", map[string]string{"docid": id}, &result); err != nil {
		return nil, err
	}
	return result.Allow, nil
}

func (c *Client) Rename(ctx context.Context, e Entry, name string, replace bool) (Entry, error) {
	if !ValidName(name) {
		return Entry{}, errors.New("invalid filename")
	}
	kind := "file"
	if e.Dir {
		kind = "dir"
	}
	ondup := 1
	if replace {
		ondup = 3
	}
	var out Entry
	if err := c.post(ctx, "/efast/v1/"+kind+"/rename", map[string]any{"docid": e.ID, "name": name, "ondup": ondup}, &out); err != nil {
		return Entry{}, err
	}
	if out.ID == "" {
		out.ID = e.ID
	}
	if out.Rev == "" {
		out.Rev = e.Rev
	}
	out.Dir, out.Size, out.Name = e.Dir, e.Size, name
	return out, nil
}

func (c *Client) Move(ctx context.Context, e Entry, parent string) (Entry, error) {
	kind := "file"
	if e.Dir {
		kind = "dir"
	}
	var out Entry
	if err := c.post(ctx, "/efast/v1/"+kind+"/move", map[string]any{"docid": e.ID, "destparent": parent, "ondup": 1, "check_upload_process": true}, &out); err != nil {
		return Entry{}, err
	}
	if out.ID == "" {
		return Entry{}, errors.New("move response omitted the new document ID")
	}
	out.Name, out.Dir, out.Size = e.Name, e.Dir, e.Size
	if out.Rev == "" {
		out.Rev = e.Rev
	}
	return out, nil
}

// Resolve accepts a display path or a gns:// ID, and confines it to accessible libraries.
func (c *Client) Resolve(ctx context.Context, selector string) (Entry, error) {
	roots, err := c.Roots(ctx)
	if err != nil {
		return Entry{}, err
	}
	if strings.HasPrefix(selector, "gns://") {
		for _, root := range roots {
			if LibraryID(selector) != root.ID {
				continue
			}
			current := root
			if selector == root.ID {
				return current, nil
			}
			parts := strings.Split(strings.TrimPrefix(selector, root.ID+"/"), "/")
			for _, component := range parts {
				if component == "" || component == "." || component == ".." {
					return Entry{}, ErrNotFound
				}
				entries, err := c.List(ctx, current.ID)
				if err != nil {
					return Entry{}, err
				}
				found := false
				for _, e := range entries {
					if e.ID == current.ID+"/"+component {
						current = e
						found = true
						break
					}
				}
				if !found {
					return Entry{}, ErrNotFound
				}
			}
			return current, nil
		}
		return Entry{}, ErrPermission
	}
	parts := strings.Split(strings.Trim(selector, "/"), "/")
	var current Entry
	for _, root := range roots {
		if root.Name == parts[0] {
			current = root
			break
		}
	}
	if current.ID == "" {
		return Entry{}, ErrNotFound
	}
	for _, name := range parts[1:] {
		if !ValidName(name) || !current.Dir {
			return Entry{}, ErrNotFound
		}
		entries, err := c.List(ctx, current.ID)
		if err != nil {
			return Entry{}, err
		}
		found := false
		for _, e := range entries {
			if e.Name == name {
				current = e
				found = true
				break
			}
		}
		if !found {
			return Entry{}, ErrNotFound
		}
	}
	return current, nil
}
