package sandpod

import (
	"testing"
	"time"
)

func TestSandboxStorePagination(t *testing.T) {
	s := NewSandboxStore()
	base := time.Now().Add(-time.Hour)
	for i, owner := range []string{"a", "a", "b", "a"} {
		if err := s.Add(&SandboxInfo{Name: string(rune('a' + i)), DisplayName: string(rune('a' + i)), Owner: owner, CreatedAt: base.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	page := s.ListPage(2, 1)
	if len(page) != 2 || page[0].Name != "c" || page[1].Name != "b" {
		t.Fatalf("ListPage = %+v", page)
	}
	ownerPage := s.ListByOwnerPage("a", 2, 0)
	if len(ownerPage) != 2 || ownerPage[0].Name != "d" || ownerPage[1].Name != "b" {
		t.Fatalf("ListByOwnerPage = %+v", ownerPage)
	}
}
