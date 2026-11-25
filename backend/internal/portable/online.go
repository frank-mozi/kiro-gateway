package portable

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const defaultRepository = "https://github.com/frank-mozi/kiro-gateway"
const defaultPublisher = "CTTCG81qGWaR0zrZNvpdKGkoYGdmFUuugNx4EGsjLuE="

var githubRepository = regexp.MustCompile(`^https://github\.com/[a-zA-Z0-9][a-zA-Z0-9-]*/[a-zA-Z0-9_.-]+$`)

func onlineSource(s Source) (Source, error) {
	s.Repository = strings.TrimSuffix(strings.TrimSpace(s.Repository), "/")
	s.PublicKey = strings.TrimSpace(s.PublicKey)
	if s.Directory != "" || !githubRepository.MatchString(s.Repository) || strings.HasSuffix(s.Repository, "/.") || strings.HasSuffix(s.Repository, "/..") {
		return Source{}, errors.New("expected a public HTTPS GitHub repository URL: https://github.com/owner/repo")
	}
	if key, err := base64.StdEncoding.DecodeString(s.PublicKey); err != nil || len(key) != ed25519.PublicKeySize {
		return Source{}, errors.New("expected a base64 Ed25519 publisher public key")
	}
	return s, nil
}

func storedSource(s Source) (Source, error) {
	if s.Repository == "" && (s.Directory != "" || s.PublicKey == "") {
		// A local feed's key must never implicitly become trusted for a remote origin.
		return Source{Repository: defaultRepository, PublicKey: defaultPublisher}, nil
	}
	return onlineSource(s)
}

func releaseRedirect(req *http.Request, via []*http.Request) error {
	u := req.URL
	if len(via) > 6 || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return errors.New("unexpected GitHub release redirect")
	}
	switch u.Host {
	case "github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com", "github-releases.githubusercontent.com":
		return nil
	}
	return errors.New("redirect outside GitHub release hosts")
}

var releaseHTTP = &http.Client{CheckRedirect: releaseRedirect, Timeout: 5 * time.Minute}

func fetchReleaseFile(ctx context.Context, client *http.Client, address string, limit int64, out io.Writer) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		// HTTP errors can include a signed CDN URL. Do not expose its query string.
		var e *url.Error
		if errors.As(err, &e) {
			err = e.Err
		}
		return 0, fmt.Errorf("GitHub Releases request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("GitHub Releases returned HTTP %d; check the public repository and published release assets", resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return 0, errors.New("GitHub release file exceeds size limit")
	}
	n, err := io.Copy(out, io.LimitReader(resp.Body, limit+1))
	if n > limit {
		return n, errors.New("GitHub release file exceeds size limit")
	}
	return n, err
}

func onlineRelease(ctx context.Context, client *http.Client, s Source) (Release, error) {
	s, err := onlineSource(s)
	if err != nil {
		return Release{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var buf strings.Builder
	if _, err := fetchReleaseFile(ctx, client, s.Repository+"/releases/latest/download/manifest.json", maxManifest, &buf); err != nil {
		return Release{}, err
	}
	return verifyManifest([]byte(buf.String()), s.PublicKey)
}

func stageOnline(ctx context.Context, client *http.Client, root string, s Source, release Release) (string, error) {
	s, err := onlineSource(s)
	if err != nil {
		return "", err
	}
	a, err := release.asset()
	if err != nil {
		return "", err
	}
	want := fmt.Sprintf("kiro-gateway_%s_%s_%s.zip", release.Version, a.OS, a.Arch)
	if a.File != want {
		return "", errors.New("unexpected platform asset filename in signed release")
	}
	dir, err := os.MkdirTemp(filepath.Join(root, "downloads"), "github-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	f, err := os.OpenFile(filepath.Join(dir, a.File), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	address := s.Repository + "/releases/download/v" + release.Version + "/" + a.File
	n, copyErr := fetchReleaseFile(ctx, client, address, a.Size, f)
	if err := errors.Join(copyErr, f.Close()); err != nil {
		return "", err
	}
	if n != a.Size {
		return "", errors.New("incomplete GitHub release download")
	}
	return stageRelease(root, Source{Directory: dir}, release)
}
