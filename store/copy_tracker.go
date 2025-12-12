package store

import (
	"database/sql"
	"strings"
	"time"
)

// CopyTrackedPosition 表示当前处于跟踪状态的 symbol/side。
type CopyTrackedPosition struct {
	TraderID     string    `json:"trader_id"`
	Symbol       string    `json:"symbol"`
	Side         string    `json:"side"`
	FollowerSize float64   `json:"follower_size"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// CopyTrackerStore 维护跟单跟踪状态，支持重启恢复。
type CopyTrackerStore struct {
	db *sql.DB
}

func (s *CopyTrackerStore) initTables() error {
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS copy_tracked_positions (
			trader_id TEXT NOT NULL,
			symbol TEXT NOT NULL,
			side TEXT NOT NULL,
			follower_size REAL DEFAULT 0,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (trader_id, symbol, side),
			FOREIGN KEY (trader_id) REFERENCES traders(id) ON DELETE CASCADE
		)
	`); err != nil {
		return err
	}
	// 旧表补充 follower_size 列
	if _, err := s.db.Exec(`ALTER TABLE copy_tracked_positions ADD COLUMN follower_size REAL DEFAULT 0`); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return err
		}
	}
	return nil
}

// Upsert 记录跟踪状态。
func (s *CopyTrackerStore) Upsert(traderID, symbol, side string, followerSize float64) error {
	if s == nil || traderID == "" || symbol == "" || side == "" {
		return nil
	}
	_, err := s.db.Exec(`
		INSERT INTO copy_tracked_positions (trader_id, symbol, side, follower_size, updated_at)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(trader_id, symbol, side)
		DO UPDATE SET follower_size=excluded.follower_size, updated_at=CURRENT_TIMESTAMP
	`, traderID, symbol, side, followerSize)
	return err
}

// Delete 删除指定跟踪状态；symbol/side 为空时删除整个 trader 的记录。
func (s *CopyTrackerStore) Delete(traderID, symbol, side string) error {
	if s == nil || traderID == "" {
		return nil
	}
	if symbol == "" || side == "" {
		_, err := s.db.Exec(`DELETE FROM copy_tracked_positions WHERE trader_id = ?`, traderID)
		return err
	}
	_, err := s.db.Exec(`DELETE FROM copy_tracked_positions WHERE trader_id = ? AND symbol = ? AND side = ?`, traderID, symbol, side)
	return err
}

// List 返回指定 trader 的跟踪状态。
func (s *CopyTrackerStore) List(traderID string) ([]CopyTrackedPosition, error) {
	if s == nil || traderID == "" {
		return nil, nil
	}
	rows, err := s.db.Query(`SELECT trader_id, symbol, side, follower_size, updated_at FROM copy_tracked_positions WHERE trader_id = ?`, traderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []CopyTrackedPosition
	for rows.Next() {
		var rec CopyTrackedPosition
		var updated string
		if err := rows.Scan(&rec.TraderID, &rec.Symbol, &rec.Side, &rec.FollowerSize, &updated); err != nil {
			return nil, err
		}
		if updated != "" {
			if ts, err := time.Parse(time.RFC3339Nano, updated); err == nil {
				rec.UpdatedAt = ts
			} else if ts, err := time.Parse("2006-01-02 15:04:05", updated); err == nil {
				rec.UpdatedAt = ts
			}
		}
		result = append(result, rec)
	}
	return result, rows.Err()
}
