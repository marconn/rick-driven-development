# Rick — Event-Driven AI Workflow System
# Usage: make help

RICK_BIN       := $(HOME)/.local/bin/rick
AGENT_BIN      := $(HOME)/.local/bin/rick-agent
AGENT_DIR      := agent
AGENT_VERSION  := 0.8.0
AGENT_DEB      := deploy/rick-agent_$(AGENT_VERSION)_amd64.deb
PLUGINS_DIR    := ../rick-plugins
JIRA_BIN       := $(HOME)/.local/bin/rick-jira-poller
REPORTER_BIN   := $(HOME)/.local/bin/rick-github-reporter
JIRA_PLANNER_BIN := $(HOME)/.local/bin/rick-jira-planner
PLANNING_BIN     := $(HOME)/.local/bin/rick-planning

.PHONY: help build build-agent build-plugins lint test deploy deploy-agent deploy-plugins restart package clean

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

# --- Build ---

build: ## Build rick server binary
	go build -o $(RICK_BIN) ./cmd/rick

build-agent: ## Build rick-agent desktop app (Wails)
	cd $(AGENT_DIR) && wails build
	cp $(AGENT_DIR)/build/bin/rick-agent $(AGENT_BIN)

build-plugins: ## Build all plugin binaries
	cd $(PLUGINS_DIR) && go build -o $(JIRA_BIN) ./cmd/jira-poller
	cd $(PLUGINS_DIR) && go build -o $(REPORTER_BIN) ./cmd/github-reporter
	cd $(PLUGINS_DIR) && go build -o $(JIRA_PLANNER_BIN) ./cmd/jira-planner
	cd $(PLUGINS_DIR) && go build -o $(PLANNING_BIN) ./cmd/planning

build-all: build build-agent build-plugins ## Build everything

# --- Quality ---

lint: ## Run golangci-lint
	golangci-lint run

test: ## Run all tests
	go test ./...

check: lint test ## Lint + test

# --- Deploy ---

deploy: build ## Build and restart rick-server
	systemctl --user restart rick-server.service
	@sleep 0.5
	@systemctl --user is-active rick-server.service --quiet && echo "✓ rick-server running" || echo "✗ rick-server failed"

deploy-plugins: build-plugins ## Build and restart plugin services
	systemctl --user restart rick-github-reporter.service
	-systemctl --user restart rick-jira-poller.service
	-systemctl --user restart rick-jira-planner.service
	-systemctl --user restart rick-planning.service
	@sleep 1
	@systemctl --user is-active rick-github-reporter.service --quiet && echo "✓ github-reporter running" || echo "✗ github-reporter failed"
	@systemctl --user is-active rick-jira-poller.service --quiet && echo "✓ jira-poller running" || echo "· jira-poller not running (check JIRA_URL/JIRA_TOKEN)"
	@systemctl --user is-active rick-jira-planner.service --quiet && echo "✓ jira-planner running" || echo "· jira-planner not running (check JIRA_URL/JIRA_TOKEN)"
	@systemctl --user is-active rick-planning.service --quiet && echo "✓ planning running" || echo "· planning not running (check CONFLUENCE_URL/HULIPATH)"

deploy-all: deploy deploy-plugins ## Deploy server + plugins

restart: ## Restart all services (no rebuild)
	systemctl --user restart rick-server.service
	systemctl --user restart rick-github-reporter.service
	-systemctl --user restart rick-jira-poller.service
	-systemctl --user restart rick-jira-planner.service
	-systemctl --user restart rick-planning.service

# --- Agent packaging ---

package: build-agent ## Build agent and create .deb package
	@rm -rf /tmp/rick-agent-deb
	@mkdir -p /tmp/rick-agent-deb/usr/bin
	@mkdir -p /tmp/rick-agent-deb/usr/share/applications
	@mkdir -p /tmp/rick-agent-deb/usr/share/icons/hicolor/scalable/apps
	@mkdir -p /tmp/rick-agent-deb/DEBIAN
	@printf 'Package: rick-agent\nVersion: $(AGENT_VERSION)\nSection: devel\nPriority: optional\nArchitecture: amd64\nDepends: libwebkit2gtk-4.1-0, libgtk-3-0\nMaintainer: Team Rocket <team-rocket@hulilabs.com>\nHomepage: https://github.com/marconn/rick-event-driven-development\nDescription: Rick Operator - Desktop AI Workflow Dashboard\n Native desktop application for managing Rick event-driven AI workflows.\n Built with Wails (Go + Svelte). Connects to rick-server via HTTP MCP\n and uses Google Gemini as the operator agent.\n .\n Requires rick-server running at localhost:58077 and GOOGLE_API_KEY set.\n' > /tmp/rick-agent-deb/DEBIAN/control
	cp $(AGENT_BIN) /tmp/rick-agent-deb/usr/bin/rick-agent
	cp deploy/rick-agent.desktop /tmp/rick-agent-deb/usr/share/applications/rick-agent.desktop
	cp deploy/rick-agent.svg /tmp/rick-agent-deb/usr/share/icons/hicolor/scalable/apps/rick-agent.svg
	dpkg-deb --build /tmp/rick-agent-deb $(AGENT_DEB)
	@rm -rf /tmp/rick-agent-deb
	@echo "✓ $(AGENT_DEB)"

install-agent: package ## Build, package, and install agent .deb
	sudo dpkg -i $(AGENT_DEB)
	@echo "✓ rick-agent installed — restart the app"

# --- Status ---

status: ## Show status of all services
	@systemctl --user status rick-server.service rick-github-reporter.service rick-jira-poller.service rick-jira-planner.service rick-planning.service 2>&1 | grep -E "^●|Active:"

logs: ## Tail logs for all services
	journalctl --user -u rick-server.service -u rick-github-reporter.service -u rick-jira-poller.service -u rick-jira-planner.service -u rick-planning.service -f

# --- Clean ---

clean: ## Remove built binaries
	rm -f rick $(AGENT_DIR)/rick-agent
