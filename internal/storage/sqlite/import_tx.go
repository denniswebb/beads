package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

// CreateIssueImport creates an issue inside an existing sqlite transaction, optionally skipping
// prefix validation. This is used by JSONL import to support multi-repo mode (GH#686).
func (t *sqliteTxStorage) CreateIssueImport(ctx context.Context, issue *types.Issue, actor string, skipPrefixValidation bool) error {
	// Fetch custom statuses and types for validation
	customStatuses, err := t.GetCustomStatuses(ctx)
	if err != nil {
		return fmt.Errorf("failed to get custom statuses: %w", err)
	}
	customTypes, err := t.GetCustomTypes(ctx)
	if err != nil {
		return fmt.Errorf("failed to get custom types: %w", err)
	}

	// Set timestamps
	now := time.Now()
	if issue.CreatedAt.IsZero() {
		issue.CreatedAt = now
	}
	if issue.UpdatedAt.IsZero() {
		issue.UpdatedAt = now
	}

	// Defensive fix for closed_at invariant
	if issue.Status == types.StatusClosed && issue.ClosedAt == nil {
		maxTime := issue.CreatedAt
		if issue.UpdatedAt.After(maxTime) {
			maxTime = issue.UpdatedAt
		}
		closedAt := maxTime.Add(time.Second)
		issue.ClosedAt = &closedAt
	}
	// Defensive fix for tombstone invariant
	if issue.Status == types.StatusTombstone && issue.DeletedAt == nil {
		maxTime := issue.CreatedAt
		if issue.UpdatedAt.After(maxTime) {
			maxTime = issue.UpdatedAt
		}
		deletedAt := maxTime.Add(time.Second)
		issue.DeletedAt = &deletedAt
	}

	// Validate issue before creating
	if err := issue.ValidateWithCustom(customStatuses, customTypes); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	// Compute content hash
	if issue.ContentHash == "" {
		issue.ContentHash = issue.ComputeContentHash()
	}

	// Get configured prefix for validation and ID generation behavior
	var configPrefix string
	err = t.conn.QueryRowContext(ctx, `SELECT value FROM config WHERE key = ?`, "issue_prefix").Scan(&configPrefix)
	if err == sql.ErrNoRows || configPrefix == "" {
		return fmt.Errorf("database not initialized: issue_prefix config is missing (run 'bd init --prefix <prefix>' first)")
	} else if err != nil {
		return fmt.Errorf("failed to get config: %w", err)
	}

	prefix := configPrefix
	if issue.IDPrefix != "" {
		prefix = configPrefix + "-" + issue.IDPrefix
	}

	if issue.ID == "" {
		// Import path expects IDs, but be defensive and generate if missing.
		generatedID, err := GenerateIssueID(ctx, t.conn, prefix, issue, actor)
		if err != nil {
			return fmt.Errorf("failed to generate issue ID: %w", err)
		}
		issue.ID = generatedID
	} else if !skipPrefixValidation {
		if err := ValidateIssueIDPrefix(issue.ID, prefix); err != nil {
			return fmt.Errorf("failed to validate issue ID prefix: %w", err)
		}
	}

	// NOTE: Parent existence for hierarchical IDs is handled by the importer
	// which sorts issues by depth (parents before children) and handles orphan
	// policies (allow/skip/strict/resurrect). We don't validate here because:
	// 1. With OrphanHandling=allow, orphans are intentionally permitted
	// 2. Within a transaction, parents inserted earlier are visible to children
	// 3. Redundant validation here would break fresh clone imports where the
	//    importer sorts by depth but this check sees an empty DB

	// Insert issue (strict)
	if err := insertIssueStrict(ctx, t.conn, issue); err != nil {
		return fmt.Errorf("failed to insert issue: %w", err)
	}
	// Record event
	if err := recordCreatedEvent(ctx, t.conn, issue, actor); err != nil {
		return fmt.Errorf("failed to record creation event: %w", err)
	}
	// Mark dirty
	if err := markDirty(ctx, t.conn, issue.ID); err != nil {
		return fmt.Errorf("failed to mark issue dirty: %w", err)
	}
	return nil
}

