package github

import (
	"reflect"
	"testing"
)

func TestParseIssueRefs(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []IssueRef
	}{
		{"empty", "", nil},
		{"no refs", "just some text", nil},
		{"bare hash", "see #642 please", []IssueRef{{Number: 642}}},
		{"repo form", "hulilabs/huli#642", []IssueRef{{Owner: "hulilabs", Repo: "huli", Number: 642}}},
		{"mixed", "depends on #642, hulilabs/huli#646 and #648", []IssueRef{
			{Number: 642},
			{Owner: "hulilabs", Repo: "huli", Number: 646},
			{Number: 648},
		}},
		{"zero rejected", "#0 and #5", []IssueRef{{Number: 5}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseIssueRefs(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ParseIssueRefs(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseDependencyTable_Huli641Shape(t *testing.T) {
	// Shape matching the validation case in the spec (§12): child rows with a
	// "Depends on" column, bare `#N` references, and one cross-repo ref.
	body := `Observability epic.

| Issue | Title                       | Depends on    |
| ----- | --------------------------- | ------------- |
| #642  | Temporal OTel interceptor   |               |
| #643  | Delete appmetrics           |               |
| #646  | Collector base              |               |
| #647  | Dashboards baseline         |               |
| #648  | Tracing wiring              |               |
| #650  | Log routing                 |               |
| #645  | Wave 2 item A               | #642          |
| #649  | Wave 2 item B               | #646          |
| #644  | Final consolidation         | #642, #646    |

Trailing notes.
`
	got := ParseDependencyTable(body)
	want := []DependencyEdge{
		{From: IssueRef{Number: 645}, On: IssueRef{Number: 642}},
		{From: IssueRef{Number: 649}, On: IssueRef{Number: 646}},
		{From: IssueRef{Number: 644}, On: IssueRef{Number: 642}},
		{From: IssueRef{Number: 644}, On: IssueRef{Number: 646}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("edges mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}

func TestParseDependencyTable_HeaderAliases(t *testing.T) {
	body := `
| Task | Blocked by |
| ---- | ---------- |
| #10  | #11, #12   |
`
	got := ParseDependencyTable(body)
	want := []DependencyEdge{
		{From: IssueRef{Number: 10}, On: IssueRef{Number: 11}},
		{From: IssueRef{Number: 10}, On: IssueRef{Number: 12}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("edges mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}

func TestParseDependencyTable_NoTable(t *testing.T) {
	if got := ParseDependencyTable("just a normal description without a table"); got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
	if got := ParseDependencyTable(""); got != nil {
		t.Fatalf("expected nil on empty, got %+v", got)
	}
}

func TestParseDependencyTable_CrossRepoRef(t *testing.T) {
	body := `
| Issue | Depends on        |
| ----- | ----------------- |
| #42   | other/repo#99     |
`
	got := ParseDependencyTable(body)
	want := []DependencyEdge{
		{From: IssueRef{Number: 42}, On: IssueRef{Owner: "other", Repo: "repo", Number: 99}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("edges mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}

func TestParseDependencyTable_MalformedRowsSkipped(t *testing.T) {
	body := `
| Issue | Depends on |
| ----- | ---------- |
| not-a-ref | #10   |
| #20   | garbage    |
| #30   | #31        |
`
	got := ParseDependencyTable(body)
	want := []DependencyEdge{
		{From: IssueRef{Number: 30}, On: IssueRef{Number: 31}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("edges mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}
