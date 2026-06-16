package baseline

import (
	"path/filepath"
	"testing"
)

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baseline.json")

	s, err := Load(path)
	if err != nil {
		t.Fatalf("load empty: %v", err)
	}
	if _, ok := s.Get(1); ok {
		t.Fatal("empty store should have no entries")
	}

	if err := s.Set(1, 26); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.Set(2, 21.5); err != nil {
		t.Fatalf("set: %v", err)
	}

	// Reload from disk and confirm persistence.
	s2, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if v, ok := s2.Get(1); !ok || v != 26 {
		t.Errorf("device 1 = %v (ok=%v), want 26", v, ok)
	}
	if v, ok := s2.Get(2); !ok || v != 21.5 {
		t.Errorf("device 2 = %v (ok=%v), want 21.5", v, ok)
	}
}

func TestLoadMissingFile(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if _, ok := s.Get(1); ok {
		t.Fatal("expected empty store")
	}
}
