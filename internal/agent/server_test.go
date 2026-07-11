package agent

import "testing"

func TestLoopback(t *testing.T) {
	if !isLoopback("127.0.0.1:8790") {
		t.Fatal("expected localhost agent bind to be loopback")
	}
	if isLoopback("0.0.0.0:8790") {
		t.Fatal("public agent bind must not be treated as loopback")
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("active\ninactive\n"); got != "active" {
		t.Fatalf("firstLine = %q, want active", got)
	}
	if got := firstLine(""); got != "unknown" {
		t.Fatalf("empty firstLine = %q, want unknown", got)
	}
}
