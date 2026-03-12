package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/confluence"
)

// --- toolConfluenceRead ---

func TestToolConfluenceRead_InvalidJSON(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	tool := s.tools["rick_confluence_read"]
	_, err := tool.Handler(t.Context(), json.RawMessage(`{bad json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "invalid arguments") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestToolConfluenceRead_APIError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/content/99999", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Page not found","statusCode":404}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := confluence.NewClient(srv.URL, "test@example.com", "token")
	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Confluence = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	_, err := callTool(t, s, "rick_confluence_read", map[string]any{
		"page_id": "99999",
	})
	if err == nil {
		t.Fatal("expected error for API 404")
	}
	if !strings.Contains(err.Error(), "read page") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestToolConfluenceRead_WithURLExtractPageID(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/content/55555", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(confluencePageResponse("55555", "Extracted Page", "<p>body</p>", 2))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := confluence.NewClient(srv.URL, "test@example.com", "token")
	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Confluence = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	// Pass a URL containing /pages/55555/ — extractPageID must extract "55555".
	result, err := callTool(t, s, "rick_confluence_read", map[string]any{
		"page_id": "https://wiki.example.com/wiki/spaces/ENG/pages/55555/Some-Title",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rm := result.(map[string]any)
	if rm["id"] != "55555" {
		t.Errorf("expected id '55555', got %v", rm["id"])
	}
	if rm["title"] != "Extracted Page" {
		t.Errorf("expected title 'Extracted Page', got %v", rm["title"])
	}
}

func TestToolConfluenceRead_ReturnsAllFields(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/content/77777", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(confluencePageResponse("77777", "Full Fields Page",
			"<h2>Section</h2><p>Detail</p>", 7))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := confluence.NewClient(srv.URL, "test@example.com", "token")
	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Confluence = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := callTool(t, s, "rick_confluence_read", map[string]any{
		"page_id": "77777",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rm := result.(map[string]any)
	if rm["id"] != "77777" {
		t.Errorf("expected id '77777', got %v", rm["id"])
	}
	if rm["version"] != 7 {
		t.Errorf("expected version 7, got %v", rm["version"])
	}
	if rm["space_key"] != "ENG" {
		t.Errorf("expected space_key 'ENG', got %v", rm["space_key"])
	}
	body, ok := rm["body"].(string)
	if !ok || body == "" {
		t.Errorf("expected non-empty body string, got %v", rm["body"])
	}
}

// --- toolConfluenceWrite ---

func TestToolConfluenceWrite_InvalidJSON(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	tool := s.tools["rick_confluence_write"]
	_, err := tool.Handler(t.Context(), json.RawMessage(`{bad`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "invalid arguments") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestToolConfluenceWrite_ReadPageAPIError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/content/88888", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"Internal server error"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := confluence.NewClient(srv.URL, "test@example.com", "token")
	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Confluence = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	_, err := callTool(t, s, "rick_confluence_write", map[string]any{
		"page_id":       "88888",
		"content":       "New content",
		"after_heading": "Section Header",
	})
	if err == nil {
		t.Fatal("expected error when ReadPage fails")
	}
	if !strings.Contains(err.Error(), "read page") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestToolConfluenceWrite_MissingPageID(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	_, err := callTool(t, s, "rick_confluence_write", map[string]any{
		"content":       "# Some content",
		"after_heading": "Plan Tecnico",
		// page_id omitted
	})
	if err == nil {
		t.Fatal("expected error for missing page_id")
	}
	if !strings.Contains(err.Error(), "page_id and content are required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestToolConfluenceWrite_SuccessReturnsPageInfo(t *testing.T) {
	const pageID = "66666"
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/content/"+pageID, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			body := `<h2>Technical Plan</h2><p>Old plan text.</p><h2>Risks</h2>`
			_ = json.NewEncoder(w).Encode(confluencePageResponse(pageID, "Sprint Planning", body, 4))
			return
		}
		// PUT update — accept it.
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": pageID})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := confluence.NewClient(srv.URL, "test@example.com", "token")
	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Confluence = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := callTool(t, s, "rick_confluence_write", map[string]any{
		"page_id":       pageID,
		"content":       "## New technical approach\n- step one\n- step two",
		"after_heading": "Technical Plan",
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
	if rm["page_id"] != pageID {
		t.Errorf("expected page_id=%q, got %v", pageID, rm["page_id"])
	}
	if rm["title"] != "Sprint Planning" {
		t.Errorf("expected title 'Sprint Planning', got %v", rm["title"])
	}
	if rm["heading"] != "Technical Plan" {
		t.Errorf("expected heading 'Technical Plan', got %v", rm["heading"])
	}
}

// --- extractPageID (edge cases not covered by helpers_test.go) ---

func TestExtractPageID_URLWithoutTrailingSlash(t *testing.T) {
	// URL where the page ID is the last segment (no trailing title segment).
	url := "https://wiki.example.com/pages/44444"
	id := extractPageID(url)
	if id != "44444" {
		t.Errorf("expected '44444', got %q", id)
	}
}
