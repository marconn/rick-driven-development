package pluginstore

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Store provides shared SQLite storage for cross-plugin state.
// The Jira poller writes ticket→workflow mappings; the GitHub reporter
// reads them to correlate workflows with PRs.
type Store struct {
	db *sql.DB
}

// New opens (or creates) the SQLite database at path and applies migrations.
func New(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("pluginstore: open: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("pluginstore: migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database connection.
func (s *Store) Close() error { return s.db.Close() }

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS processed_tickets (
			ticket_id      TEXT PRIMARY KEY,
			correlation_id TEXT NOT NULL,
			repo           TEXT NOT NULL DEFAULT '',
			branch         TEXT NOT NULL DEFAULT '',
			pr_url         TEXT NOT NULL DEFAULT '',
			pr_number      INTEGER NOT NULL DEFAULT 0,
			summary        TEXT NOT NULL DEFAULT '',
			status         TEXT NOT NULL DEFAULT 'pending',
			created_at     TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at     TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE INDEX IF NOT EXISTS idx_tickets_correlation ON processed_tickets(correlation_id);
		CREATE INDEX IF NOT EXISTS idx_tickets_status ON processed_tickets(status);

		CREATE TABLE IF NOT EXISTS ci_attempts (
			ticket_id  TEXT PRIMARY KEY,
			attempts   INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
	`)
	return err
}

// Ticket represents a processed Jira ticket in storage.
type Ticket struct {
	TicketID      string
	CorrelationID string
	Repo          string
	Branch        string
	PRURL         string
	PRNumber      int
	Summary       string
	Status        string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// IsProcessed returns true if the ticket has already been ingested.
func (s *Store) IsProcessed(ticketID string) (bool, error) {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM processed_tickets WHERE ticket_id = ?", ticketID).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("pluginstore: is processed: %w", err)
	}
	return count > 0, nil
}

// SaveTicket records a new processed ticket.
func (s *Store) SaveTicket(t Ticket) error {
	_, err := s.db.Exec(`
		INSERT INTO processed_tickets (ticket_id, correlation_id, repo, branch, pr_url, pr_number, summary, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(ticket_id) DO UPDATE SET
			correlation_id = excluded.correlation_id,
			status = excluded.status,
			updated_at = datetime('now')
	`, t.TicketID, t.CorrelationID, t.Repo, t.Branch, t.PRURL, t.PRNumber, t.Summary, t.Status)
	if err != nil {
		return fmt.Errorf("pluginstore: save ticket: %w", err)
	}
	return nil
}

// UpdateTicketStatus updates the workflow status for a ticket.
func (s *Store) UpdateTicketStatus(correlationID, status string) error {
	_, err := s.db.Exec(`
		UPDATE processed_tickets SET status = ?, updated_at = datetime('now')
		WHERE correlation_id = ?
	`, status, correlationID)
	if err != nil {
		return fmt.Errorf("pluginstore: update ticket status: %w", err)
	}
	return nil
}

// GetTicketByCorrelation retrieves a ticket by its workflow correlation ID.
func (s *Store) GetTicketByCorrelation(correlationID string) (*Ticket, error) {
	t := &Ticket{}
	var createdAt, updatedAt string
	err := s.db.QueryRow(`
		SELECT ticket_id, correlation_id, repo, branch, pr_url, pr_number, summary, status, created_at, updated_at
		FROM processed_tickets WHERE correlation_id = ?
	`, correlationID).Scan(&t.TicketID, &t.CorrelationID, &t.Repo, &t.Branch, &t.PRURL, &t.PRNumber, &t.Summary, &t.Status, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pluginstore: get ticket by correlation: %w", err)
	}
	t.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
	t.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05", updatedAt)
	return t, nil
}

// GetCIAttemptCount returns the number of ci-fix attempts for a ticket.
func (s *Store) GetCIAttemptCount(ticketID string) int {
	var count int
	err := s.db.QueryRow("SELECT COALESCE(attempts, 0) FROM ci_attempts WHERE ticket_id = ?", ticketID).Scan(&count)
	if err != nil {
		return 0
	}
	return count
}

// IncrementCIAttempt increments the CI fix attempt counter for a ticket.
func (s *Store) IncrementCIAttempt(ticketID string) error {
	_, err := s.db.Exec(`
		INSERT INTO ci_attempts (ticket_id, attempts) VALUES (?, 1)
		ON CONFLICT(ticket_id) DO UPDATE SET
			attempts = attempts + 1,
			updated_at = datetime('now')
	`, ticketID)
	if err != nil {
		return fmt.Errorf("pluginstore: increment ci attempt: %w", err)
	}
	return nil
}
