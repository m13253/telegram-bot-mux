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
		"BEGIN; " +
			"CREATE TABLE IF NOT EXISTS chats (id INTEGER PRIMARY KEY, chat JSONB NOT NULL); " +
			"CREATE TABLE IF NOT EXISTS messages (id INTEGER PRIMARY KEY, chat_id INTEGER NOT NULL, message_id INTEGER NOT NULL, message JSONB NOT NULL, UNIQUE(chat_id, message_id)); " +
			"CREATE TABLE IF NOT EXISTS updates (id INTEGER PRIMARY KEY, upstream_id INTEGER UNIQUE, type TEXT NOT NULL, \"update\" JSONB NOT NULL); " +
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
	// Some client libraries poll from 0, other poll from 1, so we start our real updates from update_id = 2
	stmt, err := d.conn.PrepareContext(ctx, "SELECT id + 1 FROM updates ORDER BY id DESC LIMIT 1;")
	if err != nil {
		return 0, fmt.Errorf("database error: %v", err)
	}
	var id uint64
	err = stmt.QueryRowContext(ctx).Scan(&id)
	stmt.Close()
	if err != nil {
		if err == sql.ErrNoRows {
			return 1, nil
		}
		return 0, fmt.Errorf("database error: %v", err)
	}
	return id, nil
}

func (d *Database) GetUpdates(ctx context.Context, offset int64, limit uint64) iter.Seq2[string, error] {
	var stmt *sql.Stmt
	var err error
	if offset >= 0 {
		stmt, err = d.conn.PrepareContext(ctx, "SELECT json_object('update_id', id + 1, type, \"update\") FROM updates WHERE id >= ? - 1 ORDER BY id ASC LIMIT ?;")
	} else {
		stmt, err = d.conn.PrepareContext(ctx, "SELECT json_object('update_id', id + 1, type, \"update\") FROM (SELECT id, type, \"update\" FROM updates ORDER BY id DESC LIMIT -?) ORDER BY id ASC LIMIT ?;")
	}
	if err != nil {
		return func(yield func(string, error) bool) {
			yield("", fmt.Errorf("database error: %v", err))
		}
	}
	rows, err := stmt.QueryContext(ctx, offset, limit)
	if err != nil {
		stmt.Close()
		return func(yield func(string, error) bool) {
			yield("", fmt.Errorf("database error: %v", err))
		}
	}
	return func(yield func(string, error) bool) {
		for rows.Next() {
			var update string
			err := rows.Scan(&update)
			if err != nil {
				rows.Close()
				stmt.Close()
				yield("", fmt.Errorf("database error: %v", err))
				return
			}
			if !yield(update, nil) {
				rows.Close()
				stmt.Close()
				return
			}
		}
		err := rows.Err()
		rows.Close()
		stmt.Close()
		if err != nil {
			yield("", fmt.Errorf("database error: %v", err))
		}
	}
}

func (d *Database) GetChatType(ctx context.Context, chatID int64) (string, error) {
	stmt, err := d.conn.PrepareContext(ctx, "SELECT json_extract(chat, '$.type') FROM chats WHERE id = ?;")
	if err != nil {
		return "", fmt.Errorf("database error: %v", err)
	}
	var chatType string
	err = stmt.QueryRowContext(ctx, chatID).Scan(&chatType)
	stmt.Close()
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
	log.Printf("Inserting update %d: {%q:%s}\n", upstreamID, updateType, updateValue)
	stmt, err := tx.tx.Prepare("INSERT OR REPLACE INTO updates (upstream_id, type, \"update\") VALUES (?, ?, jsonb(?));")
	if err != nil {
		return fmt.Errorf("database error: %v", err)
	}
	result, err := stmt.Exec(upstreamID, updateType, updateValue)
	if err != nil {
		stmt.Close()
		return fmt.Errorf("database error: %v", err)
	}
	tx.setUpdatedFlag(result)
	stmt.Close()
	return nil
}

func (tx *DatabaseTx) InsertEchoUpdate(updateType, updateValue string) error {
	log.Printf("Inserting echo update: {%q:%s}\n", updateType, updateValue)
	stmt, err := tx.tx.Prepare("INSERT OR REPLACE INTO updates (type, \"update\") VALUES (?, jsonb(?));")
	if err != nil {
		return fmt.Errorf("database error: %v", err)
	}
	result, err := stmt.Exec(updateType, updateValue)
	if err != nil {
		stmt.Close()
		return fmt.Errorf("database error: %v", err)
	}
	tx.setUpdatedFlag(result)
	stmt.Close()
	return nil
}

func (tx *DatabaseTx) InsertMessage(messageJSON *gjson.Result) error {
	messageID := messageJSON.Get("message_id").Int()
	chat := messageJSON.Get("chat")
	chatID := chat.Get("id").Int()
	log.Println("Inserting message:", messageJSON)

	stmt, err := tx.tx.Prepare("INSERT OR REPLACE INTO chats (id, chat) VALUES (?, jsonb(?));")
	if err != nil {
		return fmt.Errorf("database error: %v", err)
	}
	result, err := stmt.Exec(chatID, chat.Raw)
	if err != nil {
		stmt.Close()
		return fmt.Errorf("database error: %v", err)
	}
	tx.setUpdatedFlag(result)
	stmt.Close()

	stmt, err = tx.tx.Prepare("INSERT OR REPLACE INTO messages (chat_id, message_id, message) VALUES (?, ?, jsonb(?));")
	if err != nil {
		return fmt.Errorf("database error: %v", err)
	}
	result, err = stmt.Exec(chatID, messageID, messageJSON.Raw)
	if err != nil {
		stmt.Close()
		return fmt.Errorf("database error: %v", err)
	}
	tx.setUpdatedFlag(result)
	stmt.Close()
	return nil
}

func (tx *DatabaseTx) setUpdatedFlag(result sql.Result) {
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
