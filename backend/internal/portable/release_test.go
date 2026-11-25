package portable

import (
	"archive/zip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type releaseTransport func(*http.Request) (*http.Response, error)

func (f releaseTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCheckAndStageGitHubRelease(t *testing.T) {
	_, local, _ := fixture(t)
	src := Source{Repository: "https://github.com/example/gateway", PublicKey: local.PublicKey}
	paths := []string{}
	c := &http.Client{Transport: releaseTransport(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		data, err := os.ReadFile(filepath.Join(local.Directory, filepath.Base(r.URL.Path)))
		if err != nil {
			return nil, err
		}
		w := httptest.NewRecorder()
		w.Write(data)
		return w.Result(), nil
	})}
	ctx := context.Background()
	release, err := onlineRelease(ctx, c, src)
	if err != nil || len(paths) != 1 || paths[0] != "/example/gateway/releases/latest/download/manifest.json" {
		t.Fatalf("online check: paths %v, error %v", paths, err)
	}
	root := t.TempDir()
	for _, dir := range []string{"downloads", "releases"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	asset, err := release.asset()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stageOnline(ctx, c, root, src, release); err != nil || paths[1] != "/example/gateway/releases/download/v0.3.1/"+asset.File {
		t.Fatalf("online staging: paths %v, error %v", paths, err)
	}
	put(t, filepath.Join(local.Directory, asset.File), strings.Repeat("x", int(asset.Size)))
	if _, err := stageOnline(ctx, c, root, src, release); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("expected digest rejection, got %v", err)
	}
	src.PublicKey = defaultPublisher
	if _, err := onlineRelease(ctx, c, src); err == nil {
		t.Fatal("untrusted manifest accepted")
	}
}

func TestMigrateLocalUpdateSource(t *testing.T) {
	for _, legacy := range []Source{{}, {Directory: "/old/feed", PublicKey: "previous-key"}} {
		s, err := storedSource(legacy)
		if err != nil || s.Repository != defaultRepository || s.PublicKey != defaultPublisher || s.Directory != "" {
			t.Fatal("failed to migrate local feed safely")
		}
	}
	for _, repo := range []string{"https://github.com/a/..", "https://other.example/a/b", "http://github.com/a/b", "https://github.com/a/b?token=value"} {
		if _, err := onlineSource(Source{Repository: repo, PublicKey: defaultPublisher}); err == nil {
			t.Fatalf("accepted %s", repo)
		}
	}
}

func put(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T) (string, Source, string) {
	t.Helper()
	root := t.TempDir()
	key := filepath.Join(root, "publisher.key")
	public, err := GenerateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(root, "bundle")
	put(t, filepath.Join(bundle, BinaryName(runtime.GOOS)), "fake executable")
	put(t, filepath.Join(bundle, "frontend", "dist", "index.html"), "test dashboard")
	put(t, filepath.Join(bundle, "data", "master.key"), "MUST NOT BE PACKAGED")
	source := Source{Directory: filepath.Join(root, "feed"), PublicKey: public}
	if err := Publish(bundle, source.Directory, "0.3.1", runtime.GOOS, runtime.GOARCH, key, "local test"); err != nil {
		t.Fatal(err)
	}
	return bundle, source, key
}

func TestPublishVerifyStage(t *testing.T) {
	bundle, source, key := fixture(t)
	release, err := readRelease(source)
	if err != nil {
		t.Fatal(err)
	}
	if release.Version != "0.3.1" || !newer("local-0.2.0", release.Version) || newer("dev", release.Version) || newer("0.3.1", release.Version) {
		t.Fatal("version comparison")
	}
	if err := Publish(bundle, source.Directory, "0.3.1", runtime.GOOS, runtime.GOARCH, key, "overwrite"); err == nil {
		t.Fatal("mutable release allowed")
	}
	root := t.TempDir()
	for _, dir := range []string{"downloads", "releases"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	target, err := stageRelease(root, source, release)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, target, "data")); !os.IsNotExist(err) {
		t.Fatal("runtime data entered release")
	}
	if _, err := stageRelease(root, source, release); err == nil {
		t.Fatal("existing release overwritten")
	}
	if _, err := os.Stat(filepath.Join(root, target, "frontend", "dist", "index.html")); err != nil {
		t.Fatal(err)
	}
}

func TestPublishDefaultReleaseTargets(t *testing.T) {
	root := t.TempDir()
	key := filepath.Join(root, "publisher.key")
	public, err := GenerateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	source := Source{Directory: filepath.Join(root, "feed"), PublicKey: public}
	for _, goos := range []string{"windows", "linux", "darwin"} {
		bundle := filepath.Join(root, goos)
		put(t, filepath.Join(bundle, BinaryName(goos)), "fake executable")
		put(t, filepath.Join(bundle, "frontend", "dist", "index.html"), "test dashboard")
		if err := Publish(bundle, source.Directory, "0.4.7", goos, "amd64", key, "three default platforms"); err != nil {
			t.Fatalf("publish %s/amd64: %v", goos, err)
		}
	}
	release, err := readRelease(source)
	if err != nil {
		t.Fatal(err)
	}
	if len(release.Assets) != 3 {
		t.Fatalf("expected 3 signed assets, got %d", len(release.Assets))
	}
	for i, goos := range []string{"windows", "linux", "darwin"} {
		asset := release.Assets[i]
		if asset.OS != goos || asset.Arch != "amd64" {
			t.Fatalf("unexpected asset: %+v", asset)
		}
		archive, err := zip.OpenReader(filepath.Join(source.Directory, asset.File))
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, file := range archive.File {
			legacyPath := "keirouter/keirouter"
			if goos == "windows" {
				legacyPath += ".exe"
			}
			if file.Name == legacyPath {
				found = true
				if goos != "windows" && file.Mode().Perm()&0111 == 0 {
					t.Errorf("%s executable has no execute permission", goos)
				}
			}
		}
		archive.Close()
		if !found {
			t.Errorf("%s archive is missing the executable", goos)
		}
	}
}

func TestRejectTamperedRelease(t *testing.T) {
	_, source, _ := fixture(t)
	release, err := readRelease(source)
	if err != nil {
		t.Fatal(err)
	}
	asset, err := release.asset()
	if err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(source.Directory, asset.File), "corrupted")
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "downloads"), 0700)
	os.Mkdir(filepath.Join(root, "releases"), 0700)
	if _, err := stageRelease(root, source, release); err == nil {
		t.Fatal("corrupt archive accepted")
	}
	path := filepath.Join(source.Directory, "manifest.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var signed envelope
	if err := json.Unmarshal(raw, &signed); err != nil {
		t.Fatal(err)
	}
	signed.Payload = []byte(strings.Replace(string(signed.Payload), "0.3.1", "9.9.9", 1))
	if err := writeJSONFile(path, signed); err != nil {
		t.Fatal(err)
	}
	if _, err := readRelease(source); err == nil {
		t.Fatal("invalid signature accepted")
	}
}

func TestUnsafeArchives(t *testing.T) {
	for _, name := range []string{"../outside", "keirouter/../../outside", "keirouter/C:/outside", `keirouter/a\b`, "keirouter/CON", "keirouter/x.", "other/file"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "bad.zip")
			file, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			w := zip.NewWriter(file)
			entry, err := w.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			entry.Write([]byte("bad"))
			w.Close()
			file.Close()
			if err := extract(path, filepath.Join(root, "out")); err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
}

func TestRestorePreservesFailedData(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "data", "keirouter.db"), "migrated")
	put(t, filepath.Join(root, "data", "master.key"), "original key")
	tx := transaction{Previous: ".", Backup: filepath.Join("backups", "test", "data"), Failed: filepath.Join("backups", "test", "failed-data")}
	put(t, filepath.Join(root, tx.Backup, "keirouter.db"), "old database")
	put(t, filepath.Join(root, tx.Backup, "master.key"), "original key")
	os.Mkdir(filepath.Join(root, "downloads"), 0700)
	if err := writeJSONFile(filepath.Join(root, "transaction.json"), tx); err != nil {
		t.Fatal(err)
	}
	m := &Manager{root: root}
	if err := m.recoverTransaction(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "data", "keirouter.db"))
	if err != nil || string(raw) != "old database" {
		t.Fatalf("restore: %q, %v", raw, err)
	}
	raw, err = os.ReadFile(filepath.Join(root, tx.Failed, "keirouter.db"))
	if err != nil || string(raw) != "migrated" {
		t.Fatalf("failed data not retained: %q, %v", raw, err)
	}
	if p, err := m.readPointer(); err != nil || p != "." {
		t.Fatalf("pointer %q: %v", p, err)
	}
	if _, err := os.Stat(filepath.Join(root, "transaction.json")); !os.IsNotExist(err) {
		t.Fatal("recovery journal not committed")
	}
}
