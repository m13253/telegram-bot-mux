package main

import (
	"log"
	"sync"
	"time"
)

type CooldownQueueItem struct {
	notify   chan<- struct{}
	cooldown time.Duration
}

type CooldownQueue struct {
	mtx            sync.Mutex
	queue          map[uint64]CooldownQueueItem
	front          uint64
	back           uint64
	lastPop        time.Time
	interruptWaker chan<- struct{}
	wakerRunning   bool
}

func NewCooldownQueue() *CooldownQueue {
	return &CooldownQueue{
		mtx:            sync.Mutex{},
		queue:          make(map[uint64]CooldownQueueItem),
		front:          0,
		back:           0,
		lastPop:        time.Time{},
		interruptWaker: nil,
		wakerRunning:   false,
	}
}

func (q *CooldownQueue) Push(cooldown time.Duration) (notify <-chan struct{}, cancel func()) {
	c := make(chan struct{})
	q.mtx.Lock()
	token := q.back
	q.back++
	q.queue[token] = CooldownQueueItem{notify: c, cooldown: cooldown}
	if !q.wakerRunning {
		q.wakerRunning = true
		go q.waker()
	}
	q.mtx.Unlock()
	return c, func() {
		q.mtx.Lock()
		delete(q.queue, token)
		if q.interruptWaker != nil {
			close(q.interruptWaker)
		}
		q.mtx.Unlock()
	}
}

func (q *CooldownQueue) waker() {
	q.mtx.Lock()
	for q.front < q.back {
		if item, ok := q.queue[q.front]; ok {
			var dur time.Duration
			if !q.lastPop.IsZero() {
				dur = time.Until(q.lastPop.Add(item.cooldown))
			}
			if dur <= 0 {
				q.lastPop = time.Now()
				close(item.notify)
				delete(q.queue, q.front)
				q.front++
			} else {
				sleep := time.After(dur)
				interrupt := make(chan struct{})
				q.interruptWaker = interrupt
				q.mtx.Unlock()
				log.Println("Cooldown", dur)
				select {
				case <-sleep:
					q.mtx.Lock()
					q.interruptWaker = nil
					q.lastPop = time.Now()
					close(item.notify)
					delete(q.queue, q.front)
					q.front++
				case <-interrupt:
					q.mtx.Lock()
					q.interruptWaker = nil
				}
			}
		} else {
			q.front++
		}
	}
	q.wakerRunning = false
	q.mtx.Unlock()
}
