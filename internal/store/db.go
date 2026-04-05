package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	_ "github.com/lib/pq"
	"go.mau.fi/whatsmeow/store/sqlstore"
)

// DB holds the raw sql.DB and whatsmeow container.
type DB struct {
	SQL       *sql.DB
	Container *sqlstore.Container
}

// Connect opens a PostgreSQL connection and initializes the whatsmeow device store.
func Connect(databaseURL string) (*DB, error) {
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Connection pooling for whatsmeow + gateway queries
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	// Create tables for our gateway metadata
	if _, err := db.Exec(instancesTableSQL); err != nil {
		return nil, fmt.Errorf("failed to create instances table: %w", err)
	}
	if _, err := db.Exec(deadLetterTableSQL); err != nil {
		return nil, fmt.Errorf("failed to create dead_letters table: %w", err)
	}

	container := sqlstore.NewWithDB(db, "postgres", nil)
	if err := container.Upgrade(context.Background()); err != nil {
		return nil, fmt.Errorf("failed to upgrade whatsmeow store: %w", err)
	}

	slog.Info("database connected and whatsmeow store initialized")
	return &DB{SQL: db, Container: container}, nil
}

const instancesTableSQL = `
CREATE TABLE IF NOT EXISTS gateway_instances (
	id            TEXT PRIMARY KEY,
	name          TEXT NOT NULL UNIQUE,
	organization_id TEXT NOT NULL,
	jid           TEXT,
	phone_number  TEXT,
	status        TEXT NOT NULL DEFAULT 'disconnected',
	created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`

const deadLetterTableSQL = `
CREATE TABLE IF NOT EXISTS webhook_dead_letters (
	id         BIGSERIAL PRIMARY KEY,
	event_type TEXT NOT NULL,
	instance_id TEXT NOT NULL,
	payload    JSONB NOT NULL,
	error      TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`

// Instance represents a WhatsApp connection managed by this gateway.
type Instance struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	OrganizationID string `json:"organizationId"`
	JID            string `json:"jid,omitempty"`
	PhoneNumber    string `json:"phoneNumber,omitempty"`
	Status         string `json:"status"`
	CreatedAt      string `json:"createdAt"`
	UpdatedAt      string `json:"updatedAt"`
}

func (db *DB) CreateInstance(id, name, orgID string) (*Instance, error) {
	inst := &Instance{ID: id, Name: name, OrganizationID: orgID, Status: "disconnected"}
	_, err := db.SQL.Exec(
		`INSERT INTO gateway_instances (id, name, organization_id, status) VALUES ($1, $2, $3, $4)`,
		inst.ID, inst.Name, inst.OrganizationID, inst.Status,
	)
	if err != nil {
		return nil, err
	}
	return inst, nil
}

func (db *DB) GetInstance(id string) (*Instance, error) {
	inst := &Instance{}
	err := db.SQL.QueryRow(
		`SELECT id, name, organization_id, COALESCE(jid,''), COALESCE(phone_number,''), status, created_at::text, updated_at::text FROM gateway_instances WHERE id = $1`,
		id,
	).Scan(&inst.ID, &inst.Name, &inst.OrganizationID, &inst.JID, &inst.PhoneNumber, &inst.Status, &inst.CreatedAt, &inst.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return inst, nil
}

func (db *DB) UpdateInstanceStatus(id, status string) error {
	_, err := db.SQL.Exec(
		`UPDATE gateway_instances SET status = $1, updated_at = NOW() WHERE id = $2`,
		status, id,
	)
	return err
}

func (db *DB) UpdateInstanceJID(id, jid, phone string) error {
	_, err := db.SQL.Exec(
		`UPDATE gateway_instances SET jid = $1, phone_number = $2, updated_at = NOW() WHERE id = $3`,
		jid, phone, id,
	)
	return err
}

func (db *DB) DeleteInstance(id string) error {
	_, err := db.SQL.Exec(`DELETE FROM gateway_instances WHERE id = $1`, id)
	return err
}

func (db *DB) ListInstances(orgID string) ([]Instance, error) {
	rows, err := db.SQL.Query(
		`SELECT id, name, organization_id, COALESCE(jid,''), COALESCE(phone_number,''), status, created_at::text, updated_at::text FROM gateway_instances WHERE organization_id = $1 ORDER BY created_at`,
		orgID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var instances []Instance
	for rows.Next() {
		var inst Instance
		if err := rows.Scan(&inst.ID, &inst.Name, &inst.OrganizationID, &inst.JID, &inst.PhoneNumber, &inst.Status, &inst.CreatedAt, &inst.UpdatedAt); err != nil {
			return nil, err
		}
		instances = append(instances, inst)
	}
	return instances, nil
}

func (db *DB) ListAllInstances() ([]Instance, error) {
	rows, err := db.SQL.Query(
		`SELECT id, name, organization_id, COALESCE(jid,''), COALESCE(phone_number,''), status, created_at::text, updated_at::text FROM gateway_instances ORDER BY created_at`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var instances []Instance
	for rows.Next() {
		var inst Instance
		if err := rows.Scan(&inst.ID, &inst.Name, &inst.OrganizationID, &inst.JID, &inst.PhoneNumber, &inst.Status, &inst.CreatedAt, &inst.UpdatedAt); err != nil {
			return nil, err
		}
		instances = append(instances, inst)
	}
	return instances, nil
}

// InsertDeadLetter persists a failed webhook event for later inspection/retry.
func (db *DB) InsertDeadLetter(eventType, instanceID string, payload []byte, errMsg string) error {
	_, err := db.SQL.Exec(
		`INSERT INTO webhook_dead_letters (event_type, instance_id, payload, error) VALUES ($1, $2, $3, $4)`,
		eventType, instanceID, payload, errMsg,
	)
	return err
}

// CountInstancesByOrg returns the number of instances for an organization.
func (db *DB) CountInstancesByOrg(orgID string) (int, error) {
	var count int
	err := db.SQL.QueryRow(
		`SELECT COUNT(*) FROM gateway_instances WHERE organization_id = $1`, orgID,
	).Scan(&count)
	return count, err
}

func (db *DB) Close() error {
	return db.SQL.Close()
}
