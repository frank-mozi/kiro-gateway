// Package portable provides a user-owned launcher and signed local updates.
package portable

import (
	"archive/zip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/mydisha/keirouter/backend/internal/update"
)

const (
	Product     = "kiro-gateway-portable"
	maxArchive  = 256 << 20
	maxExpanded = 768 << 20
	maxManifest = 1 << 20
)

var stableVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var reservedName = regexp.MustCompile(`^(COM|LPT)[0-9]$`)

type Source struct {
	Repository string `json:"repository,omitempty"`
	Directory  string `json:"directory,omitempty"` // Legacy settings and local publishing.
	PublicKey  string `json:"public_key"`
}

type Asset struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type Release struct {
	Schema      int     `json:"schema"`
	Product     string  `json:"product"`
	Version     string  `json:"version"`
	PublishedAt string  `json:"published_at"`
	Notes       string  `json:"notes"`
	Assets      []Asset `json:"assets"`
}

type envelope struct {
	Payload   []byte `json:"payload"`
	Signature []byte `json:"signature"`
}

type bundleInfo struct {
	Product string `json:"product"`
	Version string `json:"version"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

func BinaryName(goos string) string {
	if goos == "windows" {
		return "kiro-gateway.exe"
	}
	return "kiro-gateway"
}

// Schema 1 launchers validate and start this exact filename inside signed feeds.
func feedBinaryName(goos string) string {
	if goos == "windows" {
		return "keirouter.exe"
	}
	return "keirouter"
}

func newer(current, candidate string) bool {
	current = strings.TrimPrefix(current, "local-")
	return stableVersion.MatchString(current) && stableVersion.MatchString(candidate) && update.IsNewer(current, candidate)
}

func validateSource(s Source) error {
	if !filepath.IsAbs(s.Directory) {
		return errors.New("release directory must be an absolute local path, not a SharePoint browser URL")
	}
	info, err := os.Stat(s.Directory)
	if err != nil {
		return fmt.Errorf("release directory: %w", err)
	}
	if !info.IsDir() {
		return errors.New("release source is not a directory")
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s.PublicKey))
	if err != nil || len(key) != ed25519.PublicKeySize {
		return errors.New("publisher public key must be a base64 Ed25519 public key")
	}
	return nil
}

func readRelease(s Source) (Release, error) {
	var release Release
	if err := validateSource(s); err != nil {
		return release, err
	}
	raw, err := readLimited(filepath.Join(s.Directory, "manifest.json"), maxManifest)
	if err != nil {
		return release, fmt.Errorf("read release manifest: %w", err)
	}
	return verifyManifest(raw, s.PublicKey)
}

func verifyManifest(raw []byte, publicKey string) (Release, error) {
	var release Release
	var signed envelope
	if err := json.Unmarshal(raw, &signed); err != nil {
		return release, fmt.Errorf("decode signed manifest: %w", err)
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(publicKey))
	if err != nil || len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, signed.Payload, signed.Signature) {
		return release, errors.New("release signature is invalid; refusing this update")
	}
	if err := json.Unmarshal(signed.Payload, &release); err != nil {
		return release, fmt.Errorf("decode release: %w", err)
	}
	if release.Schema != 1 || release.Product != Product || !stableVersion.MatchString(release.Version) {
		return release, errors.New("unsupported release schema, product or version")
	}
	if _, err := time.Parse(time.RFC3339, release.PublishedAt); err != nil {
		return release, errors.New("invalid publication time")
	}
	seen := map[string]bool{}
	for _, a := range release.Assets {
		hash, err := hex.DecodeString(a.SHA256)
		platform := a.OS + "-" + a.Arch
		if !safeName(a.File) || !strings.HasSuffix(a.File, ".zip") || a.Size <= 0 || a.Size > maxArchive ||
			err != nil || len(hash) != sha256.Size || seen[platform] {
			return release, errors.New("invalid or duplicate release asset")
		}
		seen[platform] = true
	}
	return release, nil
}

func (r Release) asset() (Asset, error) {
	for _, a := range r.Assets {
		if a.OS == runtime.GOOS && a.Arch == runtime.GOARCH {
			return a, nil
		}
	}
	return Asset{}, fmt.Errorf("release has no package for %s/%s", runtime.GOOS, runtime.GOARCH)
}

func safeName(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, `/\:`) && !strings.ContainsRune(s, 0)
}

func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errors.New("file exceeds size limit")
	}
	return raw, nil
}

func writeJSONFile(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, raw)
}

func atomicWrite(path string, raw []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return replaceFile(name, path)
}

func GenerateKey(path string) (string, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	_, writeErr := f.WriteString(base64.StdEncoding.EncodeToString(private))
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(public), nil
}

// Publish only includes application files, never the portable data/config tree.
func Publish(bundle, directory, version, goos, arch, keyPath, notes string) error {
	if !stableVersion.MatchString(version) || (goos != "linux" && goos != "windows" && goos != "darwin") || (arch != "amd64" && arch != "arm64") {
		return errors.New("expected a stable X.Y.Z version and linux/windows/darwin amd64/arm64 target")
	}
	privatePath, err := filepath.Abs(keyPath)
	if err != nil {
		return err
	}
	privatePath, err = filepath.EvalSymlinks(privatePath)
	if err != nil {
		return err
	}
	for _, directory := range []string{bundle, directory} {
		base, err := filepath.Abs(directory)
		if err != nil {
			return err
		}
		if resolved, err := filepath.EvalSymlinks(base); err == nil {
			base = resolved
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		relative, err := filepath.Rel(base, privatePath)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
			return errors.New("signing key must be outside the bundle and release directories")
		}
	}
	rawKey, err := readLimited(keyPath, 4096)
	if err != nil {
		return err
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(rawKey)))
	if err != nil || len(key) != ed25519.PrivateKeySize {
		return errors.New("invalid Ed25519 signing key")
	}
	private := ed25519.PrivateKey(key)
	public := base64.StdEncoding.EncodeToString(private.Public().(ed25519.PublicKey))
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	directory, err = filepath.Abs(directory)
	if err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(directory, ".publish.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := lockFile(lock); err != nil {
		return errors.New("another publisher is writing this feed")
	}
	release := Release{Schema: 1, Product: Product, Version: version, PublishedAt: time.Now().UTC().Format(time.RFC3339), Notes: notes}
	if _, err := os.Stat(filepath.Join(directory, "manifest.json")); err == nil {
		old, err := readRelease(Source{Directory: directory, PublicKey: public})
		if err != nil {
			return err
		}
		if old.Version == version {
			release = old
		} else if !newer(old.Version, version) {
			return errors.New("cannot publish an older release")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, a := range release.Assets {
		if a.OS == goos && a.Arch == arch {
			return errors.New("this version/target is immutable; publish a new version")
		}
	}
	name := fmt.Sprintf("kiro-gateway_%s_%s_%s.zip", version, goos, arch)
	path := filepath.Join(directory, name)
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		out.Close()
		if !complete {
			os.Remove(path)
		}
	}()
	zw := zip.NewWriter(out)
	for _, required := range []string{BinaryName(goos), filepath.Join("frontend", "dist", "index.html")} {
		if info, err := os.Stat(filepath.Join(bundle, required)); err != nil || !info.Mode().IsRegular() {
			zw.Close()
			return fmt.Errorf("required bundle file is missing: %s", required)
		}
	}
	entries := []string{BinaryName(goos), filepath.Join("frontend", "dist"), "LICENSE", "DEPLOYMENT.md", "dependency-notices", "build-info.txt"}
	for _, top := range entries {
		base := filepath.Join(bundle, top)
		if _, err := os.Lstat(base); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		err := filepath.Walk(base, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if info.IsDir() {
				return nil
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("bundle contains non-regular file: %s", path)
			}
			relative, err := filepath.Rel(bundle, path)
			if err != nil {
				return err
			}
			entry := filepath.ToSlash(relative)
			if relative == BinaryName(goos) {
				entry = feedBinaryName(goos)
			}
			h := &zip.FileHeader{Name: "keirouter/" + entry, Method: zip.Deflate}
			h.SetMode(0600)
			if goos != "windows" && relative == BinaryName(goos) {
				h.SetMode(0700)
			}
			w, err := zw.CreateHeader(h)
			if err != nil {
				return err
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(w, f)
			return errors.Join(copyErr, f.Close())
		})
		if err != nil {
			zw.Close()
			return err
		}
	}
	w, err := zw.Create("keirouter/release.json")
	if err != nil {
		return err
	}
	if err := json.NewEncoder(w).Encode(bundleInfo{Product, version, goos, arch}); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	asset, err := hashAsset(path, goos, arch)
	if err != nil {
		return err
	}
	release.Assets = append(release.Assets, asset)
	payload, err := json.Marshal(release)
	if err != nil {
		return err
	}
	if err := writeJSONFile(filepath.Join(directory, "manifest.json"), envelope{payload, ed25519.Sign(private, payload)}); err != nil {
		return err
	}
	complete = true
	return nil
}

func hashAsset(path, goos, arch string) (Asset, error) {
	f, err := os.Open(path)
	if err != nil {
		return Asset{}, err
	}
	defer f.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(f, maxArchive+1))
	if err != nil {
		return Asset{}, err
	}
	if size == 0 || size > maxArchive {
		return Asset{}, errors.New("package size is outside supported bounds")
	}
	return Asset{goos, arch, filepath.Base(path), hex.EncodeToString(hash.Sum(nil)), size}, nil
}

func stageRelease(root string, source Source, release Release) (string, error) {
	asset, err := release.asset()
	if err != nil {
		return "", err
	}
	work, err := os.MkdirTemp(filepath.Join(root, "downloads"), "stage-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	input, err := os.Open(filepath.Join(source.Directory, asset.File))
	if err != nil {
		return "", err
	}
	output, err := os.OpenFile(filepath.Join(work, "package.zip"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		input.Close()
		return "", err
	}
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(output, hash), io.LimitReader(input, asset.Size+1))
	err = errors.Join(copyErr, input.Close(), output.Close())
	if err != nil {
		return "", err
	}
	if size != asset.Size || hex.EncodeToString(hash.Sum(nil)) != strings.ToLower(asset.SHA256) {
		return "", errors.New("package size or SHA-256 mismatch")
	}
	unpacked := filepath.Join(work, "unpacked")
	if err := extract(filepath.Join(work, "package.zip"), unpacked); err != nil {
		return "", err
	}
	bundle := filepath.Join(unpacked, "keirouter")
	raw, err := readLimited(filepath.Join(bundle, "release.json"), 4096)
	if err != nil {
		return "", err
	}
	var info bundleInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return "", err
	}
	if info != (bundleInfo{Product, release.Version, runtime.GOOS, runtime.GOARCH}) {
		return "", errors.New("package metadata does not match signed release")
	}
	for _, path := range []string{feedBinaryName(runtime.GOOS), filepath.Join("frontend", "dist", "index.html")} {
		info, err := os.Stat(filepath.Join(bundle, path))
		if err != nil || !info.Mode().IsRegular() {
			return "", fmt.Errorf("missing required file: %s", path)
		}
	}
	if err := os.Chmod(filepath.Join(bundle, feedBinaryName(runtime.GOOS)), 0700); err != nil {
		return "", err
	}
	target := filepath.Join("releases", release.Version)
	if _, err := os.Lstat(filepath.Join(root, target)); !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("target release directory already exists; it will not be overwritten")
	}
	if err := os.Rename(bundle, filepath.Join(root, target)); err != nil {
		return "", err
	}
	return target, nil
}

func extract(archive, target string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()
	if len(zr.File) > 20000 {
		return errors.New("too many archive entries")
	}
	var total uint64
	seen := map[string]bool{}
	for _, f := range zr.File {
		name := strings.TrimSuffix(f.Name, "/")
		parts := strings.Split(name, "/")
		if len(parts) < 1 || parts[0] != "keirouter" {
			return errors.New("unexpected archive root")
		}
		for _, part := range parts {
			if !safeName(part) || strings.TrimRight(part, ". ") != part {
				return errors.New("unsafe archive entry")
			}
			base := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
			if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || reservedName.MatchString(base) {
				return errors.New("reserved archive entry")
			}
		}
		if f.Mode()&os.ModeSymlink != 0 || (!f.FileInfo().IsDir() && !f.Mode().IsRegular()) {
			return errors.New("archive links and special files are not allowed")
		}
		lower := strings.ToLower(name)
		if seen[lower] {
			return errors.New("duplicate archive entry")
		}
		seen[lower] = true
		path := filepath.Join(target, filepath.FromSlash(name))
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(path, 0700); err != nil {
				return err
			}
			continue
		}
		if f.UncompressedSize64 > maxExpanded || total > maxExpanded-f.UncompressedSize64 {
			return errors.New("expanded archive is too large")
		}
		total += f.UncompressedSize64
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		in, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			in.Close()
			return err
		}
		n, copyErr := io.Copy(out, io.LimitReader(in, int64(f.UncompressedSize64)+1))
		err = errors.Join(copyErr, in.Close(), out.Close())
		if err != nil {
			return err
		}
		if uint64(n) != f.UncompressedSize64 {
			return errors.New("archive entry size mismatch")
		}
	}
	return nil
}
