package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/confluence"
)

// mockConfluenceServer creates a fake Confluence REST API server.
type mockConfluenceServer struct {
	mux    *http.ServeMux
	server *httptest.Server
}

func newMockConfluenceServer(t *testing.T) (*mockConfluenceServer, *confluence.Client) {
	t.Helper()
	mux := http.NewServeMux()
	srv := &mockConfluenceServer{mux: mux}
	srv.server = httptest.NewServer(mux)
	t.Cleanup(srv.server.Close)

	client := confluence.NewClient(srv.server.URL, "test@example.com", "token")
	return srv, client
}

// confluencePageResponse returns a fake Confluence page JSON response.
func confluencePageResponse(id, title, body string, version int) map[string]any {
	return map[string]any{
		"id":    id,
		"title": title,
		"body": map[string]any{
			"storage": map[string]any{
				"value": body,
			},
		},
		"version": map[string]any{"number": version},
		"space":   map[string]any{"key": "ENG"},
	}
}

// --- Confluence Read tests ---

func TestToolConfluenceRead_WithMockServer(t *testing.T) {
	mockSrv, client := newMockConfluenceServer(t)

	mockSrv.mux.HandleFunc("/rest/api/content/12345", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(confluencePageResponse("12345", "My Test Page",
			"<h2>Introduction</h2><p>Content here.</p>", 3))
	})

	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Confluence = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := callTool(t, s, "rick_confluence_read", map[string]any{
		"page_id": "12345",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rm, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected type: %T", result)
	}
	if rm["id"] != "12345" {
		t.Errorf("expected id '12345', got %v", rm["id"])
	}
	if rm["title"] != "My Test Page" {
		t.Errorf("expected title 'My Test Page', got %v", rm["title"])
	}
	if rm["version"] != 3 {
		t.Errorf("expected version 3, got %v", rm["version"])
	}
}

func TestToolConfluenceRead_WithURL(t *testing.T) {
	mockSrv, client := newMockConfluenceServer(t)

	mockSrv.mux.HandleFunc("/rest/api/content/98765", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(confluencePageResponse("98765", "Page From URL", "<p>body</p>", 1))
	})

	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Confluence = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	// Pass a Confluence URL — extractPageID should extract the numeric ID.
	confluenceURL := mockSrv.server.URL + "/wiki/spaces/ENG/pages/98765/Page-Title"
	result, err := callTool(t, s, "rick_confluence_read", map[string]any{
		"page_id": confluenceURL,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rm := result.(map[string]any)
	if rm["id"] != "98765" {
		t.Errorf("expected id '98765', got %v", rm["id"])
	}
}

// --- Confluence Write tests ---

func TestToolConfluenceWrite_WithMockServer(t *testing.T) {
	mockSrv, client := newMockConfluenceServer(t)

	const pageID = "54321"
	pageBody := `<h2>Plan Tecnico</h2><p>old content</p><h2>Next Section</h2>`

	// READ: returns current page content.
	mockSrv.mux.HandleFunc("/rest/api/content/"+pageID, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(confluencePageResponse(pageID, "Planning Page", pageBody, 5))
			return
		}
		// PUT: accept the update.
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": pageID})
	})

	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Confluence = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := callTool(t, s, "rick_confluence_write", map[string]any{
		"page_id":       pageID,
		"content":       "<p>New technical plan content</p>",
		"after_heading": "Plan Tecnico",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rm, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected type: %T", result)
	}
	if rm["updated"] != true {
		t.Errorf("expected updated=true")
	}
	if rm["heading"] != "Plan Tecnico" {
		t.Errorf("expected heading 'Plan Tecnico', got %v", rm["heading"])
	}
}

func TestToolConfluenceWrite_HeadingNotFound(t *testing.T) {
	mockSrv, client := newMockConfluenceServer(t)

	mockSrv.mux.HandleFunc("/rest/api/content/11111", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(confluencePageResponse("11111", "Page", "<p>No headings here</p>", 1))
	})

	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Confluence = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	_, err := callTool(t, s, "rick_confluence_write", map[string]any{
		"page_id":       "11111",
		"content":       "new content",
		"after_heading": "Nonexistent Section",
	})
	if err == nil {
		t.Fatal("expected error when heading not found")
	}
	// Should contain heading-not-found info.
	if !strings.Contains(err.Error(), "heading") && !strings.Contains(err.Error(), "not found") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestToolConfluenceWrite_MissingRequiredFields(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	// Missing content.
	_, err := callTool(t, s, "rick_confluence_write", map[string]any{
		"page_id":       "12345",
		"after_heading": "Section",
	})
	if err == nil {
		t.Fatal("expected error for missing content")
	}
}

func TestToolConfluenceWrite_MissingAfterHeading(t *testing.T) {
	mockSrv, client := newMockConfluenceServer(t)
	mockSrv.mux.HandleFunc("/rest/api/content/22222", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(confluencePageResponse("22222", "Page", "<p>body</p>", 1))
	})

	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Confluence = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	// Empty after_heading should fail with "after_heading is required" error.
	_, err := callTool(t, s, "rick_confluence_write", map[string]any{
		"page_id": "22222",
		"content": "some content",
		// after_heading intentionally omitted
	})
	if err == nil {
		t.Fatal("expected error when after_heading is missing")
	}
}
