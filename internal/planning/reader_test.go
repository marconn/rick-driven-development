package planning

import (
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/confluence"
)

func TestParseBTU_ExtractsSections(t *testing.T) {
	// Minimal HTML simulating a Confluence BTU page.
	html := `<h2>Que es</h2>
<p>A feature that allows uploading insurance PDFs.</p>
<h2>Como Funciona</h2>
<p>User uploads a PDF, system validates and stores it.</p>
<h2>Tipos de Usuario</h2>
<p>Doctor, Paciente</p>
<h2>Dispositivo</h2>
<p>Mobile, Desktop</p>
<h2>Hipotesis a Validar</h2>
<p>Users prefer drag-and-drop upload.</p>`

	r := &ReaderHandler{}
	page := &confluence.Page{ID: "123", Title: "BTU-1234", Body: html}
	btu := r.parseBTU(page)

	if btu.content == "" {
		t.Error("expected non-empty content")
	}
	if btu.userTypes == "" {
		t.Error("expected non-empty userTypes")
	}
	if btu.devices == "" {
		t.Error("expected non-empty devices")
	}
}

func TestDetectMicroservices_BracketPatterns(t *testing.T) {
	tests := []struct {
		name  string
		html  string
		want  int
		names []string
	}{
		{
			name:  "two microservices in brackets",
			html:  "Changes needed in [frontend-emr] and [bff-emr] services.",
			want:  2,
			names: []string{"frontend-emr", "bff-emr"},
		},
		{
			name: "deduplicates repeated mentions",
			html: "First [practice-web], then again [practice-web].",
			want: 1,
		},
		{
			name: "ignores common Spanish words",
			html: "[nombre] [ejemplo] [frontend-emr]",
			want: 1,
		},
		{
			name: "no brackets",
			html: "Plain text without any bracket patterns.",
			want: 0,
		},
		{
			name:  "api and service suffixes",
			html:  "[backend-api] and [auth-service]",
			want:  2,
			names: []string{"backend-api", "auth-service"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectMicroservices(tt.html)
			if len(got) != tt.want {
				t.Errorf("detectMicroservices() = %v (len %d), want len %d", got, len(got), tt.want)
			}
			for _, name := range tt.names {
				found := false
				for _, g := range got {
					if g == name {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected %q in result %v", name, got)
				}
			}
		})
	}
}

func TestIsNumeric(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"12345", true},
		{"0", true},
		{"", false},
		{"12a34", false},
		{"abc", false},
		{" 123", false},
	}

	for _, tt := range tests {
		got := isNumeric(tt.input)
		if got != tt.want {
			t.Errorf("isNumeric(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestExtractConfluencePageID(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"https://wiki.example.com/pages/12345/My+Page", "12345"},
		{"/pages/98765", "98765"},
		{"no page id here", ""},
		{"", ""},
		{"https://wiki.example.com/display/TEAM/Page+Title", ""},
	}

	for _, tt := range tests {
		got := extractConfluencePageID(tt.input)
		if got != tt.want {
			t.Errorf("extractConfluencePageID(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
