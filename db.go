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
	tx *sql.Tx
}

func OpenDatabase(conf *Config) (*Database, error) {
	conn, err := sql.Open("sqlite3", conf.DB)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %v", err)
	}
	_, err = conn.Exec(
		"BEGIN;" +
			"CREATE TABLE IF NOT EXISTS updates (id INTEGER PRIMARY KEY, upstream_id INTEGER UNIQUE, type TEXT NOT NULL, \"update\" JSONB NOT NULL);" +
			"CREATE TABLE IF NOT EXISTS messages (id INTEGER PRIMARY KEY, message_id INTEGER NOT NULL, message_thread_id INTEGER, chat_id INTEGER NOT NULL, message JSONB NOT NULL);" +
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

func (d *Database) SubscribeNextUpdate() (<-chan struct{}, func()) {
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

func (d *Database) GetUpdates(ctx context.Context, offset int64, limit uint64) iter.Seq2[string, error] {
	var rows *sql.Rows
	var err error
	if offset > 0 {
		rows, err = d.conn.QueryContext(ctx, "SELECT id, type, json(\"update\") FROM updates WHERE id >= ? ORDER BY id ASC LIMIT ?;", offset, limit)
	} else {
		rows, err = d.conn.QueryContext(ctx, "SELECT id, type, json(\"update\") FROM (SELECT * FROM updates ORDER BY id DESC LIMIT ?) ORDER BY id ASC LIMIT ?;", -offset, limit)
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

			updateJSON := fmt.Sprintf("{\"update_id\":%d,%s:%s}", id, JSONQuote(updateType), updateValue)
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

func (d *Database) BeginTx() (DatabaseTx, error) {
	var tx DatabaseTx
	var err error
	tx.tx, err = d.conn.Begin()
	return tx, err
}

func (tx *DatabaseTx) Commit() error {
	return tx.tx.Commit()
}

func (tx *DatabaseTx) InsertMessage(messageJSON *gjson.Result) error {
	messageID := messageJSON.Get("message_id").Int()
	messageThreadID := messageJSON.Get("message_thread_id")
	messageThreadIDSQL := sql.NullInt64{
		Int64: messageThreadID.Int(),
		Valid: messageThreadID.Exists(),
	}
	chatID := messageJSON.Get("chat.id").Int()
	log.Println("Inserting message:", messageJSON)
	_, err := tx.tx.Exec(
		"INSERT OR REPLACE INTO messages (message_id, message_thread_id, chat_id, message) VALUES (?, ?, ?, jsonb(?));",
		messageID, messageThreadIDSQL, chatID, messageJSON.Raw,
	)
	if err != nil {
		return fmt.Errorf("database error: %v", err)
	}
	return nil
}

func (tx *DatabaseTx) InsertUpdate(upstreamID uint64, updateType string, updateValue string) error {
	log.Printf("Inserting update %d: {\"%s\":%s}\n", upstreamID, updateType, updateValue)
	_, err := tx.tx.Exec(
		"INSERT OR REPLACE INTO updates (upstream_id, type, \"update\") VALUES (?, ?, jsonb(?));",
		upstreamID, updateType, updateValue,
	)
	if err != nil {
		return fmt.Errorf("database error: %v", err)
	}
	return nil
}

func (tx *DatabaseTx) InsertLocalUpdate(updateType string, updateValue string) error {
	log.Printf("Inserting local update: {\"%s\":%s}\n", updateType, updateValue)
	_, err := tx.tx.Exec(
		"INSERT OR REPLACE INTO updates (type, \"update\") VALUES (?, jsonb(?));",
		updateType, updateValue,
	)
	if err != nil {
		return fmt.Errorf("database error: %v", err)
	}
	return nil
}

func (tx *DatabaseTx) InsertLocalUpdateByID(messageID int64, chatID int64) error {
	fmt.Println("Inserting update by message ID", messageID, chatID)
	_, err := tx.tx.Exec(
		"INSERT OR REPLACE INTO updates (type, \"update\") SELECT ('message', message) FROM messages WHERE message_id = ? AND chat_id = ?;",
		messageID, chatID,
	)
	if err != nil {
		return fmt.Errorf("database error: %v", err)
	}
	return nil
}
