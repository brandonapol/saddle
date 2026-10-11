package release

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Repo is where releases are published.
const Repo = "brandonapol/saddle"

// Release is a published GitHub Release.
type Release struct {
	Tag     string  `json:"tag_name"`
	Body    string  `json:"body"`
	HTMLURL string  `json:"html_url"`
	Assets  []Asset `json:"assets"`
}

// Asset is one file attached to a release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// Source finds and downloads releases; tests fake it.
type Source interface {
	Latest(ctx context.Context) (Release, error)
	Download(ctx context.Context, url string) ([]byte, error)
}

// GitHub reads releases from the GitHub REST API. Token, when set (from
// GH_TOKEN or GITHUB_TOKEN), lifts the anonymous rate limit.
type GitHub struct {
	API   string // default https://api.github.com
	Repo  string // default Repo
	Token string
	HTTP  *http.Client
}

func (g GitHub) client() *http.Client {
	if g.HTTP != nil {
		return g.HTTP
	}
	return http.DefaultClient
}

func (g GitHub) get(ctx context.Context, url, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	if g.Token != "" && strings.HasPrefix(url, g.api()) {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	resp, err := g.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 256<<20))
}

func (g GitHub) api() string {
	if g.API != "" {
		return strings.TrimRight(g.API, "/")
	}
	return "https://api.github.com"
}

// Latest returns the newest published, non-prerelease release.
func (g GitHub) Latest(ctx context.Context) (Release, error) {
	repo := g.Repo
	if repo == "" {
		repo = Repo
	}
	b, err := g.get(ctx, g.api()+"/repos/"+repo+"/releases/latest", "application/vnd.github+json")
	if err != nil {
		return Release{}, fmt.Errorf("find the latest saddle release: %w", err)
	}
	var r Release
	if err := json.Unmarshal(b, &r); err != nil {
		return Release{}, err
	}
	return r, nil
}

// Download fetches a release asset.
func (g GitHub) Download(ctx context.Context, url string) ([]byte, error) {
	return g.get(ctx, url, "application/octet-stream")
}
