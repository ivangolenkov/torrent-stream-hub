package engine

import (
	"os"
	"testing"
)

func TestGetFreeSpace(t *testing.T) {
	// Use current directory for testing
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Failed to get current directory: %v", err)
	}

	space, err := GetFreeSpace(dir)
	if err != nil {
		t.Fatalf("GetFreeSpace returned error: %v", err)
	}

	if space == 0 {
		t.Errorf("GetFreeSpace returned 0, expected > 0 for a valid directory")
	}

	// Test invalid directory
	_, err = GetFreeSpace("/non/existent/path/for/test/12345")
	if err == nil {
		t.Errorf("Expected an error for non-existent path, got nil")
	}
}
