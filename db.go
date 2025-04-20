package main

import (
	"context"
	"database/sql"
	"fmt"
	"iter"
	"log"
	"sync"

	_ "github.com/mattn/go-sqlite3"
	"github.com/tidwall/gjson"
)

type Database struct {
	conn            *sql.DB
	updateMutex     *sync.Mutex
	updateQueue     map[uint64]chan<- struct{}
	nextCancelToken uint64
}

type DatabaseTx struct {
	db      *Database
	tx      *sql.Tx
	updated bool
}

func OpenDatabase(conf *Config) (*Database, error) {
	conn, err := sql.Open("sqlite3", conf.DB)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %v", err)
	}
	_, err = conn.Exec(
		"BEGIN;" +
			"CREATE TABLE IF NOT EXISTS chats (id INTEGER PRIMARY KEY, type TEXT NOT NULL);" +
			"CREATE TABLE IF NOT EXISTS messages (id INTEGER PRIMARY KEY, message_id INTEGER NOT NULL, message_thread_id INTEGER, chat_id INTEGER NOT NULL, message JSONB NOT NULL);" +
			"CREATE TABLE IF NOT EXISTS updates (id INTEGER PRIMARY KEY, upstream_id INTEGER UNIQUE, type TEXT NOT NULL, \"update\" JSONB NOT NULL);" +
			"CREATE INDEX IF NOT EXISTS idx_message_chat_id ON messages (chat_id, message_thread_id, message_id);" +
			"COMMIT;")
	if err != nil {
		return nil, fmt.Errorf("failed to write to database: %v", err)
	}
	return &Database{
		conn:        conn,
		updateQueue: make(map[uint64]chan<- struct{}),
		updateMutex: new(sync.Mutex),
	}, nil
}

func (d *Database) SubscribeNextUpdate() (notify <-chan struct{}, cancel func()) {
	c := make(chan struct{})
	d.updateMutex.Lock()
	token := d.nextCancelToken
	d.nextCancelToken++
	d.updateQueue[token] = c
	d.updateMutex.Unlock()
	return c, func() {
		d.updateMutex.Lock()
		delete(d.updateQueue, token)
		d.updateMutex.Unlock()
	}
}

func (d *Database) NotifyUpdates() {
	d.updateMutex.Lock()
	for _, v := range d.updateQueue {
		close(v)
	}
	clear(d.updateQueue)
	d.updateMutex.Unlock()
}

func (d *Database) GetLastUpdateID(ctx context.Context) (uint64, error) {
	var id uint64
	err := d.conn.QueryRowContext(ctx, "SELECT id FROM updates ORDER BY id DESC LIMIT 1;").Scan(&id)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, fmt.Errorf("database error: %v", err)
	}
	return id, nil
}

func (d *Database) GetUpdates(ctx context.Context, offset int64, limit uint64) iter.Seq2[string, error] {
	var rows *sql.Rows
	var err error
	if offset > 0 {
		rows, err = d.conn.QueryContext(ctx, "SELECT id, type, json(\"update\") FROM updates WHERE id >= ? ORDER BY id ASC LIMIT ?;", offset, limit)
	} else {
		rows, err = d.conn.QueryContext(ctx, "SELECT id, type, json(\"update\") FROM (SELECT id, type, \"update\" FROM updates ORDER BY id DESC LIMIT ?) ORDER BY id ASC LIMIT ?;", -offset, limit)
	}
	if err != nil {
		return func(yield func(string, error) bool) {
			yield("", fmt.Errorf("database error: %v", err))
		}
	}
	return func(yield func(string, error) bool) {
		for rows.Next() {
			var id uint64
			var updateType, updateValue string
			err := rows.Scan(&id, &updateType, &updateValue)
			if err != nil {
				yield("", fmt.Errorf("database error: %v", err))
				rows.Close()
				return
			}

			updateJSON := fmt.Sprintf("{\"update_id\":%d,%s:%s}", id, quoteJSON(updateType), updateValue)
			if !yield(updateJSON, nil) {
				rows.Close()
				return
			}
		}
		err := rows.Err()
		if err != nil {
			yield("", fmt.Errorf("database error: %v", err))
		}
		rows.Close()
	}
}

func (d *Database) GetChatType(ctx context.Context, chatID int64) (string, error) {
	var chatType string
	err := d.conn.QueryRowContext(ctx, "SELECT type FROM chats WHERE id = ?;", chatID).Scan(&chatType)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", fmt.Errorf("database error: %v", err)
	}
	return chatType, nil
}

func (d *Database) BeginTx() (DatabaseTx, error) {
	tx := DatabaseTx{
		db:      d,
		tx:      nil,
		updated: false,
	}
	var err error
	tx.tx, err = d.conn.Begin()
	return tx, err
}

func (tx *DatabaseTx) InsertUpdate(upstreamID uint64, updateType, updateValue string) error {
	log.Printf("Inserting update %d: {\"%s\":%s}\n", upstreamID, updateType, updateValue)
	result, err := tx.tx.Exec(
		"INSERT OR REPLACE INTO updates (upstream_id, type, \"update\") VALUES (?, ?, jsonb(?));",
		upstreamID, updateType, updateValue,
	)
	if err != nil {
		return fmt.Errorf("database error: %v", err)
	}
	tx.setUpdated(result)
	return nil
}

func (tx *DatabaseTx) InsertLocalUpdate(updateType, updateValue string) error {
	log.Printf("Inserting local update: {\"%s\":%s}\n", updateType, updateValue)
	result, err := tx.tx.Exec(
		"INSERT OR REPLACE INTO updates (type, \"update\") VALUES (?, jsonb(?));",
		updateType, updateValue,
	)
	if err != nil {
		return fmt.Errorf("database error: %v", err)
	}
	tx.setUpdated(result)
	return nil
}

func (tx *DatabaseTx) InsertMessage(messageJSON *gjson.Result) error {
	messageID := messageJSON.Get("message_id").Int()
	messageThreadID := messageJSON.Get("message_thread_id")
	messageThreadIDSQL := sql.NullInt64{
		Int64: messageThreadID.Int(),
		Valid: messageThreadID.Exists(),
	}
	chatID := messageJSON.Get("chat.id").Int()
	chatType := messageJSON.Get("chat.type").String()
	log.Println("Inserting message:", messageJSON)
	result, err := tx.tx.Exec(
		"INSERT OR REPLACE INTO chats (id, type) VALUES (?, ?);"+
			"INSERT OR REPLACE INTO messages (message_id, message_thread_id, chat_id, message) VALUES (?, ?, ?, jsonb(?));",
		chatID, chatType, messageID, messageThreadIDSQL, chatID, messageJSON.Raw,
	)
	if err != nil {
		return fmt.Errorf("database error: %v", err)
	}
	tx.setUpdated(result)
	return nil
}

func (tx *DatabaseTx) setUpdated(result sql.Result) {
	rows, err := result.RowsAffected()
	tx.updated = tx.updated || (err == nil && rows != 0)
}

func (tx *DatabaseTx) Commit() error {
	err := tx.tx.Commit()
	if tx.updated {
		tx.db.NotifyUpdates()
	}
	return err
}
