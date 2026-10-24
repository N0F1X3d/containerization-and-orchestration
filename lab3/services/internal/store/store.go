// Package store — общий доступ к postgres для api и worker.
package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Order struct {
	ID          int64      `json:"id"`
	Item        string     `json:"item"`
	CreatedAt   time.Time  `json:"created_at"`
	Processed   bool       `json:"processed"`
	ProcessedAt *time.Time `json:"processed_at,omitempty"`
}

type Store struct{ pool *pgxpool.Pool }

// dbURL берёт адрес базы из DATABASE_URL или из файла DATABASE_URL_FILE.
// В кластере это файл из Secret <cluster>-app (ключ uri), примонтированного томом:
// пока оператор не создал Secret, файла нет; появится — kubelet сам подложит его в под.
func dbURL() (string, error) {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		return url, nil
	}
	if path := os.Getenv("DATABASE_URL_FILE"); path != "" {
		b, err := os.ReadFile(path)
		if err == nil && len(b) > 0 {
			return strings.TrimSpace(string(b)), nil
		}
		return "", fmt.Errorf("no database url in %s yet", path)
	}
	return "", errors.New("DATABASE_URL / DATABASE_URL_FILE not set")
}

// Connect ждёт, пока появится адрес базы и сама база ответит, — сервис стартует раньше postgres.
func Connect(ctx context.Context, log *slog.Logger) (*Store, error) {
	for {
		s, err := tryConnect(ctx)
		if err == nil {
			log.Info("connected to postgres")
			return s, nil
		}
		log.Warn("waiting for postgres", "error", err.Error())
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

func tryConnect(ctx context.Context) (*Store, error) {
	url, err := dbURL()
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	s := &Store{pool: pool}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS orders (
		id           bigserial PRIMARY KEY,
		item         text        NOT NULL,
		created_at   timestamptz NOT NULL DEFAULT now(),
		processed    boolean     NOT NULL DEFAULT false,
		processed_at timestamptz
	)`)
	return err
}

func (s *Store) Create(ctx context.Context, item string) (Order, error) {
	var o Order
	err := s.pool.QueryRow(ctx,
		`INSERT INTO orders (item) VALUES ($1) RETURNING id, item, created_at, processed, processed_at`, item).
		Scan(&o.ID, &o.Item, &o.CreatedAt, &o.Processed, &o.ProcessedAt)
	return o, err
}

func (s *Store) List(ctx context.Context, limit int) ([]Order, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, item, created_at, processed, processed_at FROM orders ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	orders := []Order{}
	for rows.Next() {
		var o Order
		if err := rows.Scan(&o.ID, &o.Item, &o.CreatedAt, &o.Processed, &o.ProcessedAt); err != nil {
			return nil, err
		}
		orders = append(orders, o)
	}
	return orders, rows.Err()
}

// Pending — сколько заказов ещё не обработано (для метрики очереди).
func (s *Store) Pending(ctx context.Context) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM orders WHERE NOT processed`).Scan(&n)
	return n, err
}

// ProcessBatch помечает обработанными до limit заказов. FOR UPDATE SKIP LOCKED —
// чтобы две реплики worker не взяли один и тот же заказ.
func (s *Store) ProcessBatch(ctx context.Context, limit int) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE orders SET processed = true, processed_at = now()
		WHERE id IN (SELECT id FROM orders WHERE NOT processed ORDER BY id LIMIT $1 FOR UPDATE SKIP LOCKED)`, limit)
	return tag.RowsAffected(), err
}

func (s *Store) Close() { s.pool.Close() }
