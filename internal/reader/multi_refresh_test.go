package reader_test

import (
	"testing"

	"github.com/garywhat/devinmonitor/internal/reader"
)

func TestMultiRefresh(t *testing.T) {
	r, err := reader.Open("")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer r.Close()
	m, ok := r.(*reader.MultiReader)
	if !ok {
		t.Skipf("not multi-source on this host: %T", r)
	}
	ss, err := r.Sessions()
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}
	if len(ss) == 0 {
		t.Fatal("no sessions")
	}
	if err := m.Refresh(); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	ss2, err := r.Sessions()
	if err != nil {
		t.Fatalf("sessions after refresh: %v", err)
	}
	if len(ss2) == 0 {
		t.Fatal("no sessions after refresh")
	}
	t.Logf("before=%d after=%d sources=%v", len(ss), len(ss2), m.Sources())
}
