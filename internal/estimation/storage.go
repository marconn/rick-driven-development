// Package estimation tracks historical estimates and calibration adjustments
// for Fibonacci story point estimation.
package estimation

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// Store manages estimation history and calibration in SQLite.
type Store struct {
	db *sql.DB
}

// NewStore opens (or creates) the estimation database at the given path.
func NewStore(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("estimation: open db: %w", err)
	}

	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}

	return &Store{db: db}, nil
}

// Close closes the database connection.
func (s *Store) Close() error { return s.db.Close() }

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS estimation_history (
			id              TEXT PRIMARY KEY,
			ticket_id       TEXT NOT NULL,
			task_description TEXT NOT NULL,
			microservice    TEXT,
			category        TEXT,
			estimated_points INTEGER NOT NULL,
			actual_points   INTEGER,
			correlation_id  TEXT,
			estimated_at    TEXT NOT NULL,
			actual_at       TEXT,
			notes           TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_est_ticket ON estimation_history(ticket_id);
		CREATE INDEX IF NOT EXISTS idx_est_micro ON estimation_history(microservice);
		CREATE INDEX IF NOT EXISTS idx_est_category ON estimation_history(category);

		CREATE TABLE IF NOT EXISTS calibration_adjustments (
			category        TEXT PRIMARY KEY,
			bias_direction  TEXT,
			avg_deviation   REAL,
			sample_count    INTEGER DEFAULT 0,
			last_updated    TEXT
		);
	`)
	if err != nil {
		return fmt.Errorf("estimation: migrate: %w", err)
	}
	return nil
}

// Estimate represents a single task estimation record.
type Estimate struct {
	ID              string
	TicketID        string
	TaskDescription string
	Microservice    string
	Category        string // "frontend", "backend", "infra"
	EstimatedPoints int
	ActualPoints    *int // nil until filled post-sprint
	CorrelationID   string
	EstimatedAt     time.Time
	ActualAt        *time.Time
	Notes           string
}

// SaveBatch persists multiple estimates in a single transaction.
func (s *Store) SaveBatch(ctx context.Context, estimates []Estimate) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("estimation: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO estimation_history
			(id, ticket_id, task_description, microservice, category,
			 estimated_points, correlation_id, estimated_at, notes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("estimation: prepare: %w", err)
	}
	defer stmt.Close()

	now := time.Now().Format(time.RFC3339)
	for i := range estimates {
		est := &estimates[i]
		if est.ID == "" {
			est.ID = uuid.New().String()
		}
		_, err := stmt.ExecContext(ctx, est.ID, est.TicketID, est.TaskDescription,
			est.Microservice, est.Category, est.EstimatedPoints,
			est.CorrelationID, now, est.Notes)
		if err != nil {
			return fmt.Errorf("estimation: insert %s: %w", est.TaskDescription, err)
		}
	}
	return tx.Commit()
}

// CalibrationSummary returns a human-readable summary of calibration state.
func (s *Store) CalibrationSummary(ctx context.Context) (string, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT category, bias_direction, avg_deviation, sample_count FROM calibration_adjustments ORDER BY sample_count DESC")
	if err != nil {
		return "", fmt.Errorf("estimation: query calibration: %w", err)
	}
	defer rows.Close()

	var sb strings.Builder
	count := 0
	for rows.Next() {
		var category, bias string
		var avgDev float64
		var samples int
		if err := rows.Scan(&category, &bias, &avgDev, &samples); err != nil {
			continue
		}
		if count == 0 {
			sb.WriteString("Historical calibration adjustments (actual vs estimated):\n")
		}
		fmt.Fprintf(&sb, "- %s: tendency to %s-estimate by %.1f points (based on %d samples)\n",
			category, bias, abs(avgDev), samples)
		count++
	}
	if count == 0 {
		return "No calibration data available yet. Use base estimation rules.", nil
	}
	return sb.String(), rows.Err()
}

// SimilarEstimates returns past estimates for similar tasks to inform estimation.
func (s *Store) SimilarEstimates(ctx context.Context, microservice, category string) (string, error) {
	var rows *sql.Rows
	var err error

	if microservice != "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT task_description, estimated_points, actual_points
			FROM estimation_history
			WHERE microservice = ? AND actual_points IS NOT NULL
			ORDER BY estimated_at DESC LIMIT 10`, microservice)
	} else if category != "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT task_description, estimated_points, actual_points
			FROM estimation_history
			WHERE category = ? AND actual_points IS NOT NULL
			ORDER BY estimated_at DESC LIMIT 10`, category)
	} else {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("estimation: query similar: %w", err)
	}
	defer rows.Close()

	var sb strings.Builder
	count := 0
	for rows.Next() {
		var desc string
		var estimated int
		var actual sql.NullInt64
		if err := rows.Scan(&desc, &estimated, &actual); err != nil {
			continue
		}
		if count == 0 {
			sb.WriteString("Similar past estimates:\n")
		}
		if actual.Valid {
			fmt.Fprintf(&sb, "- %q: estimated=%d, actual=%d\n", desc, estimated, actual.Int64)
		}
		count++
	}
	return sb.String(), rows.Err()
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
