package storage_test

import (
	"context"
	"io"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/vncwr/backwyn/internal/storage"
)

// TestS3RoundTrip exercises the storage.S3 backend against an in-process storage.S3-compatible
// server, so Put/Get/List/Stat are validated over a real HTTP + SigV4 path
// without needing live cloud credentials.
func TestS3RoundTrip(t *testing.T) {
	be := s3mem.New()
	faker := gofakes3.New(be)
	ts := httptest.NewServer(faker.Server())
	defer ts.Close()

	if err := be.CreateBucket("testbucket"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	s, err := storage.NewS3(storage.S3Options{
		Bucket:    "testbucket",
		Endpoint:  ts.URL,
		Region:    "us-east-1",
		AccessKey: "test",
		SecretKey: "test",
		PathStyle: true,
	})
	if err != nil {
		t.Fatalf("storage.NewS3: %v", err)
	}

	ctx := context.Background()

	// Put two objects.
	if err := s.Put(ctx, "manifests/b.json", strings.NewReader("second")); err != nil {
		t.Fatalf("put b: %v", err)
	}
	if err := s.Put(ctx, "manifests/a.json", strings.NewReader("first-object")); err != nil {
		t.Fatalf("put a: %v", err)
	}
	if err := s.Put(ctx, "artifacts/x.enc", strings.NewReader("blob")); err != nil {
		t.Fatalf("put x: %v", err)
	}

	// Get round-trips content.
	rc, err := s.Get(ctx, "manifests/a.json")
	if err != nil {
		t.Fatalf("get a: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("read a: %v", err)
	}
	if string(got) != "first-object" {
		t.Fatalf("get a = %q, want %q", got, "first-object")
	}

	// Stat returns size.
	sz, err := s.Stat(ctx, "manifests/a.json")
	if err != nil {
		t.Fatalf("stat a: %v", err)
	}
	if sz != int64(len("first-object")) {
		t.Fatalf("stat a = %d, want %d", sz, len("first-object"))
	}

	// List is prefix-scoped and sorted.
	keys, err := s.List(ctx, "manifests/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"manifests/a.json", "manifests/b.json"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("list = %v, want %v", keys, want)
	}
}
