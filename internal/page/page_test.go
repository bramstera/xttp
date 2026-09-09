package page

import (
	"strings"
	"testing"
)

func TestIndexEmbedded(t *testing.T) {
	if len(Index) == 0 {
		t.Fatal("index.html is empty")
	}
	if !strings.Contains(string(Index), "Hello") {
		t.Fatalf("unexpected default index.html: %q", string(Index))
	}
}
