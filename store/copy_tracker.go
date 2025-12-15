package store

import (
	"database/sql"
	"time"
)

// CopyTrackedPosition 跟单持仓跟踪记录
type CopyTrackedPosition struct {
	TraderID     string  `json:"trader_id"`
	Symbol       string  `json:"symbol"`
	Side         string  `json:"side"`
	FollowerSize float64 `json:"follower_size"`
	UpdatedAt    int64   `json:"updated_at"`
}

type CopyTrackerStore struct {
	db *sql.DB
}

func NewCopyTrackerStore(db *sql.DB) *CopyTrackerStore {
	return &CopyTrackerStore{db: db}
}

func (s *CopyTrackerStore) initTables() error {
	query := `create table if not exists copy_tracked_positions (
		trader_id text not null,
		symbol text not null,
		side text not null,
		follower_size real default 0,
		updated_at integer default 0,
		primary key (trader_id, symbol, side)
	)`
	_, err := s.db.Exec(query)
	return err
}

func (s *CopyTrackerStore) List(traderID string) ([]CopyTrackedPosition, error) {
	rows, err := s.db.Query(`select trader_id, symbol, side, follower_size, updated_at from copy_tracked_positions where trader_id=?`, traderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []CopyTrackedPosition
	for rows.Next() {
		var p CopyTrackedPosition
		if err := rows.Scan(&p.TraderID, &p.Symbol, &p.Side, &p.FollowerSize, &p.UpdatedAt); err != nil {
			return nil, err
		}
		list = append(list, p)
	}
	return list, nil
}

func (s *CopyTrackerStore) Upsert(traderID, symbol, side string, followerSize float64) error {
	now := time.Now().UnixMilli()
	query := `insert into copy_tracked_positions (trader_id, symbol, side, follower_size, updated_at) values (?, ?, ?, ?, ?)
	on conflict(trader_id, symbol, side) do update set follower_size=excluded.follower_size, updated_at=excluded.updated_at`
	_, err := s.db.Exec(query, traderID, symbol, side, followerSize, now)
	return err
}

func (s *CopyTrackerStore) Delete(traderID, symbol, side string) error {
	_, err := s.db.Exec(`delete from copy_tracked_positions where trader_id=? and symbol=? and side=?`, traderID, symbol, side)
	return err
}
