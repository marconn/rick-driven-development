package github

import (
	"reflect"
	"testing"
)

func TestParseTaskList(t *testing.T) {
	body := `
Some preamble.

- [ ] #10 first
- [x] #11 already done
- [ ] hulilabs/huli#12 cross-repo
- [ ] #10 duplicate
- [X] owner/repo#13 uppercase check
  - [ ] #14 indented
- something unrelated #99
`
	got := ParseTaskList(body)
	want := []IssueRef{
		{Number: 10},
		{Owner: "hulilabs", Repo: "huli", Number: 12},
		{Number: 14},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("refs mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}

func TestParseBodyRefs_DedupsPreservingOrder(t *testing.T) {
	body := "See #5 and owner/repo#6 — also #5 again and #7."
	got := ParseBodyRefs(body)
	want := []IssueRef{
		{Number: 5},
		{Owner: "owner", Repo: "repo", Number: 6},
		{Number: 7},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("refs mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}

func TestParseBodyDependencies(t *testing.T) {
	self := IssueRef{Owner: "o", Repo: "r", Number: 30}
	body := `
This change depends on #10 and o/r#11.

Blocked by #12.

Blocks #40, #41.

Closes #99 — should NOT become a dependency edge.
`
	got := ParseBodyDependencies(self, body)
	want := []DependencyEdge{
		{From: self, On: IssueRef{Number: 10}},
		{From: self, On: IssueRef{Owner: "o", Repo: "r", Number: 11}},
		{From: self, On: IssueRef{Number: 12}},
		{From: IssueRef{Number: 40}, On: self},
		{From: IssueRef{Number: 41}, On: self},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("edges mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}

func TestParseBodyDependencies_DropsSelfRef(t *testing.T) {
	self := IssueRef{Number: 5}
	got := ParseBodyDependencies(self, "Depends on #5, #6")
	want := []DependencyEdge{{From: self, On: IssueRef{Number: 6}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("edges mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}
