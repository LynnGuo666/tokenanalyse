package sync

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"
)

type WebDAVClient struct {
	baseURL  string
	username string
	password string
	client   *http.Client
}

func NewWebDAVClient(baseURL, username, password string) *WebDAVClient {
	return &WebDAVClient{baseURL: strings.TrimRight(baseURL, "/"), username: username,
		password: password, client: &http.Client{Timeout: 120 * time.Second}}
}

func (c *WebDAVClient) do(ctx context.Context, method, requestURL string, body io.Reader, contentType string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, requestURL, body)
	if err != nil {
		return nil, err
	}
	if c.username != "" {
		request.SetBasicAuth(c.username, c.password)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	return c.client.Do(request)
}

func (c *WebDAVClient) EnsureDir(ctx context.Context, dirURL string) error {
	response, err := c.do(ctx, "MKCOL", dirURL, nil, "")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}
	if response.StatusCode == http.StatusMethodNotAllowed || response.StatusCode == http.StatusConflict {
		return nil
	}
	return fmt.Errorf("WebDAV MKCOL %s: HTTP %d", dirURL, response.StatusCode)
}

func (c *WebDAVClient) Put(ctx context.Context, objectURL string, data []byte, contentType string) error {
	response, err := c.do(ctx, http.MethodPut, objectURL, bytes.NewReader(data), contentType)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		payload, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		return fmt.Errorf("WebDAV PUT HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(payload)))
	}
	return nil
}

func (c *WebDAVClient) Get(ctx context.Context, objectURL string) ([]byte, bool, error) {
	response, err := c.do(ctx, http.MethodGet, objectURL, nil, "")
	if err != nil {
		return nil, false, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, false, fmt.Errorf("WebDAV GET HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	return data, true, err
}

func (c *WebDAVClient) ListChildren(ctx context.Context, dirURL string) ([]string, error) {
	body := `<?xml version="1.0" encoding="utf-8"?><d:propfind xmlns:d="DAV:"><d:prop><d:resourcetype/><d:href/></d:prop></d:propfind>`
	request, err := http.NewRequestWithContext(ctx, "PROPFIND", dirURL, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	if c.username != "" {
		request.SetBasicAuth(c.username, c.password)
	}
	request.Header.Set("Depth", "1")
	request.Header.Set("Content-Type", "application/xml")
	response, err := c.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("WebDAV PROPFIND HTTP %d", response.StatusCode)
	}
	var multistatus struct {
		Responses []struct {
			Href string `xml:"href"`
		} `xml:"response"`
	}
	if err := xml.NewDecoder(response.Body).Decode(&multistatus); err != nil {
		return nil, err
	}
	dirPath := ""
	if response.Request != nil && response.Request.URL != nil {
		dirPath = strings.TrimRight(response.Request.URL.Path, "/")
	}
	names := map[string]bool{}
	for _, item := range multistatus.Responses {
		hrefPath := item.Href
		if parsed, err := url.Parse(item.Href); err == nil {
			hrefPath = parsed.Path
		} else if decoded, err := url.PathUnescape(hrefPath); err == nil {
			hrefPath = decoded
		}
		hrefPath = strings.TrimRight(hrefPath, "/")
		if dirPath != "" && hrefPath == dirPath {
			continue
		}
		name := path.Base(hrefPath)
		if name != "" && name != "." && name != "/" {
			names[name] = true
		}
	}
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result, nil
}

func (c *WebDAVClient) UsageDevicesURL(root, profile string) string {
	return c.baseURL + "/" + joinSegments(root, "usage-v1", profile, "devices") + "/"
}

func (c *WebDAVClient) DeviceURL(root, profile, deviceID string) string {
	return c.UsageDevicesURL(root, profile) + url.PathEscape(deviceID) + "/"
}

func joinSegments(parts ...string) string {
	cleaned := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.Trim(part, "/")
		if part != "" {
			cleaned = append(cleaned, url.PathEscape(part))
		}
	}
	return strings.Join(cleaned, "/")
}
