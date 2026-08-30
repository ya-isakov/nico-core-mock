package libvirt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNormalizeImageDigest(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"ABC123", "abc123"},
		{"sha256:ABC123", "abc123"},
		{"  SHA256:deadbeef  ", "deadbeef"},
	}
	for _, tc := range tests {
		if got := normalizeImageDigest(tc.in); got != tc.want {
			t.Fatalf("normalizeImageDigest(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestImageCacheHit(t *testing.T) {
	t.Parallel()

	cacheDir := t.TempDir()
	content := []byte("cached-image-data")
	hasher := sha256.New()
	hasher.Write(content)
	digest := hex.EncodeToString(hasher.Sum(nil))

	cachePath := filepath.Join(cacheDir, digest+".img")
	if err := os.WriteFile(cachePath, content, 0o644); err != nil {
		t.Fatalf("write cache file: %v", err)
	}

	size, reader, ok, err := tryOpenCachedImage(cacheDir, digest, "http://example.com/image.img")
	if err != nil {
		t.Fatalf("tryOpenCachedImage: %v", err)
	}
	if !ok {
		t.Fatal("expected cache hit")
	}
	defer reader.Close()

	if size != int64(len(content)) {
		t.Fatalf("size = %d, want %d", size, len(content))
	}
}

func TestImageCacheDigestMismatch(t *testing.T) {
	t.Parallel()

	cacheDir := t.TempDir()
	content := []byte("stale-image")
	cachePath := filepath.Join(cacheDir, "expecteddigest.img")
	if err := os.WriteFile(cachePath, content, 0o644); err != nil {
		t.Fatalf("write cache file: %v", err)
	}

	_, _, ok, err := tryOpenCachedImage(cacheDir, "expecteddigest", "http://example.com/image.img")
	if err != nil {
		t.Fatalf("tryOpenCachedImage: %v", err)
	}
	if ok {
		t.Fatal("expected cache miss after digest mismatch")
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Fatalf("expected stale cache file to be removed, stat err = %v", err)
	}
}

func TestOpenCachedOrDownloadImageSingleflight(t *testing.T) {
	content := []byte("singleflight-image-bytes")
	hasher := sha256.New()
	hasher.Write(content)
	digest := hex.EncodeToString(hasher.Sum(nil))

	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write(content)
	}))
	t.Cleanup(server.Close)

	cacheDir := t.TempDir()
	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			size, reader, err := openCachedOrDownloadImage(context.Background(), server.URL+"/image.img", digest, cacheDir)
			if err != nil {
				errs <- err
				return
			}
			defer reader.Close()
			got, err := io.ReadAll(reader)
			if err != nil {
				errs <- err
				return
			}
			if size != int64(len(content)) {
				errs <- fmt.Errorf("size = %d, want %d", size, len(content))
				return
			}
			if string(got) != string(content) {
				errs <- fmt.Errorf("unexpected content")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := downloads.Load(); got != 1 {
		t.Fatalf("expected exactly 1 download, got %d", got)
	}
}
