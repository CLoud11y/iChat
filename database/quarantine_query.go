package database

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"iChat/messaging/rabbitmq"
)

var (
	ErrQuarantineQuery    = errors.New("invalid quarantine query")
	ErrQuarantineNotFound = errors.New("quarantine audit not found")
)

// QuarantineListOptions uses the last transport ID as an exclusive cursor.
// State is open by default; all explicitly includes replay_pending/resolved.
type QuarantineListOptions struct {
	State string
	After string
	Limit int
}

func (o QuarantineListOptions) Validate() error {
	if o.Limit < 1 || o.Limit > 100 || (o.After != "" && !ValidQuarantineTransportID(o.After)) {
		return ErrQuarantineQuery
	}
	switch o.State {
	case "", "open", "replay_pending", "resolved", "all":
		return nil
	default:
		return ErrQuarantineQuery
	}
}

func ValidQuarantineTransportID(id string) bool {
	if len(id) != 70 || !strings.HasPrefix(id, "mq:v1:") {
		return false
	}
	for _, c := range id[6:] {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// QuarantineSummary contains stored metadata only. Listing does not decode or
// verify the source document; Get performs that check before returning detail.
// IDs remain strings to preserve unsigned 64-bit values in JSON consumers.
type QuarantineSummary struct {
	TransportID     string     `json:"transport_id"`
	SenderID        *string    `json:"sender_id"`
	ClientMessageID *string    `json:"client_message_id"`
	CommandID       *string    `json:"command_id"`
	PayloadHash     *string    `json:"payload_hash"`
	ErrorCode       string     `json:"error_code"`
	State           string     `json:"state"`
	ReplayAttemptID *string    `json:"replay_attempt_id"`
	CreatedAt       time.Time  `json:"created_at"`
	ResolvedAt      *time.Time `json:"resolved_at"`
	BodyBytes       int64      `json:"body_bytes"`
	AuditBytes      int64      `json:"audit_bytes"`
}

type QuarantinePage struct {
	Records []QuarantineSummary `json:"records"`
	Next    string              `json:"next_after,omitempty"`
}

type QuarantineDetail struct {
	Summary QuarantineSummary
	Input   rabbitmq.QuarantineInput
}

const listQuarantineSQL = "SELECT transport_id,sender_id,client_message_id,command_id,payload_hash,error_code,state,replay_attempt_id,DATE_FORMAT(created_at,'%Y-%m-%d %H:%i:%s.%f'),DATE_FORMAT(resolved_at,'%Y-%m-%d %H:%i:%s.%f'),OCTET_LENGTH(raw_body),OCTET_LENGTH(raw_headers) FROM message_quarantine WHERE transport_id>?"

// List performs a nonlocking SELECT, fetching one extra metadata row to detect
// another page. Pages are live views ordered by ascii_bin transport ID, not a
// cross-page snapshot or creation-time ordering.
func (r *QuarantineRepository) List(ctx context.Context, o QuarantineListOptions) (page QuarantinePage, err error) {
	if err := r.queryReady(ctx); err != nil {
		return page, err
	}
	if err := o.Validate(); err != nil {
		return page, err
	}
	state := o.State
	if state == "" {
		state = "open"
	}
	query, args := listQuarantineSQL, []any{o.After}
	if state != "all" {
		query += " AND state=?"
		args = append(args, state)
	}
	query += " ORDER BY transport_id LIMIT ?"
	args = append(args, o.Limit+1)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return page, quarantineStoreFailure("list", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, quarantineStoreFailure("close list", closeErr))
		}
		if err != nil {
			page = QuarantinePage{}
		}
	}()
	page.Records = make([]QuarantineSummary, 0, o.Limit)
	for rows.Next() {
		var row quarantineAuditRow
		var bodyBytes, auditBytes int64
		if err := rows.Scan(&row.id, &row.sender, &row.client, &row.command, &row.hash, &row.code, &row.state, &row.replay, &row.created, &row.resolved, &bodyBytes, &auditBytes); err != nil {
			return page, quarantineStoreFailure("scan list", err)
		}
		summary, err := quarantineSummary(row, bodyBytes, auditBytes)
		if err != nil {
			return page, err
		}
		if len(page.Records) == o.Limit {
			page.Next = page.Records[len(page.Records)-1].TransportID
			break
		}
		page.Records = append(page.Records, summary)
	}
	if err := rows.Err(); err != nil {
		return page, quarantineStoreFailure("iterate list", err)
	}
	if err := ctx.Err(); err != nil {
		return page, err
	}
	return page, nil
}

// Get selects exactly one audit without acquiring locks or changing state.
// It revalidates the complete document and all redundant SQL fields.
func (r *QuarantineRepository) Get(ctx context.Context, id string) (QuarantineDetail, error) {
	if err := r.queryReady(ctx); err != nil {
		return QuarantineDetail{}, err
	}
	if !ValidQuarantineTransportID(id) {
		return QuarantineDetail{}, ErrQuarantineQuery
	}
	var row quarantineAuditRow
	err := r.db.QueryRowContext(ctx, strings.TrimSuffix(readQuarantineAuditSQL, " FOR SHARE"), id).Scan(&row.id, &row.sender, &row.client, &row.command, &row.hash, &row.body, &row.document, &row.code, &row.detail, &row.state, &row.replay, &row.created, &row.resolved)
	if errors.Is(err, sql.ErrNoRows) {
		return QuarantineDetail{}, ErrQuarantineNotFound
	}
	if err != nil {
		return QuarantineDetail{}, quarantineStoreFailure("get", err)
	}
	input, err := validateQuarantineAudit(row)
	if err != nil {
		return QuarantineDetail{}, err
	}
	if row.id != id || input.Envelope().TransportID() != id {
		return QuarantineDetail{}, ErrQuarantineConflict
	}
	summary, err := quarantineSummary(row, int64(len(row.body)), int64(len(row.document)))
	if err != nil {
		return QuarantineDetail{}, err
	}
	if err := ctx.Err(); err != nil {
		return QuarantineDetail{}, err
	}
	return QuarantineDetail{Summary: summary, Input: input}, nil
}

func (r *QuarantineRepository) queryReady(ctx context.Context) error {
	if r == nil || r.db == nil {
		return ErrQuarantineRepositoryUnavailable
	}
	if ctx == nil {
		return ErrQuarantineQuery
	}
	return ctx.Err()
}

func quarantineSummary(row quarantineAuditRow, bodyBytes, auditBytes int64) (QuarantineSummary, error) {
	created, err := time.Parse(mysqlMicrosecondLayout, row.created)
	if err != nil || created.Year() < 1000 || !ValidQuarantineTransportID(row.id) || bodyBytes < 0 || auditBytes < 0 {
		return QuarantineSummary{}, ErrQuarantineInvariant
	}
	var resolved *time.Time
	if row.resolved.Valid {
		stamp, err := time.Parse(mysqlMicrosecondLayout, row.resolved.String)
		if err != nil || stamp.Year() < 1000 {
			return QuarantineSummary{}, ErrQuarantineInvariant
		}
		resolved = &stamp
	}
	return QuarantineSummary{
		TransportID: row.id, SenderID: quarantineNullable(row.sender), ClientMessageID: quarantineNullable(row.client),
		CommandID: quarantineNullable(row.command), PayloadHash: quarantineNullable(row.hash),
		ErrorCode: row.code, State: row.state, ReplayAttemptID: quarantineNullable(row.replay),
		CreatedAt: created, ResolvedAt: resolved, BodyBytes: bodyBytes, AuditBytes: auditBytes,
	}, nil
}

func quarantineNullable(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}
