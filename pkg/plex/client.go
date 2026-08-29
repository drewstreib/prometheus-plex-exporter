package plex

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const (
	// Upstream sets NEITHER of these. Both are bounds on what a wedged or misbehaving
	// Plex can do to the exporter:
	//   - no client timeout => a hung connection blocks the 5s refresh goroutine forever
	//   - unbounded io.ReadAll => the response body is read straight into memory, so an
	//     oversized reply is an OOMKill (the container limit is the only backstop)
	requestTimeout = 10 * time.Second

	// Generous: the largest real response is /media/providers, which lists libraries,
	// not items. Anything approaching this is a fault, not a big library.
	maxResponseBytes = 16 << 20
)

var ErrNotFound = errors.New("not found")

type Client struct {
	Token string
	URL   *url.URL

	httpClient http.Client
}

func NewClient(serverURL, token string) (*Client, error) {
	parsed, err := url.Parse(serverURL)
	if err != nil {
		return nil, err
	}

	client := &Client{
		Token:      token,
		URL:        parsed,
		httpClient: http.Client{Timeout: requestTimeout},
	}

	return client, nil
}

func (c *Client) NewRequest(method, path string) (*http.Request, error) {
	requestPath, err := url.Parse(path)
	if err != nil {
		return nil, err
	}

	reqURL := c.URL.ResolveReference(requestPath)
	req, err := http.NewRequest(method, reqURL.String(), nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Token", c.Token)

	return req, nil
}

func (c *Client) Do(request *http.Request, data any) error {
	resp, err := c.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return ErrNotFound
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return err
	}
	if len(body) > maxResponseBytes {
		return fmt.Errorf("response from %s exceeds %d bytes", request.URL.Path, maxResponseBytes)
	}

	return json.Unmarshal(body, &data)
}

func (c *Client) Get(path string, data any) error {
	req, err := c.NewRequest("GET", path)
	if err != nil {
		return err
	}

	return c.Do(req, &data)
}
