package repository

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const schedulerEventStreamKey = "scheduler:events"

type redisSchedulerEventBus struct {
	rdb       *redis.Client
	streamKey string
}

func NewRedisSchedulerEventBus(rdb *redis.Client) service.SchedulerEventBus {
	if rdb == nil {
		return nil
	}
	return &redisSchedulerEventBus{
		rdb:       rdb,
		streamKey: schedulerEventStreamKey,
	}
}

func (b *redisSchedulerEventBus) Publish(ctx context.Context, event service.SchedulerEvent) error {
	if b == nil || b.rdb == nil {
		return nil
	}
	if event.At.IsZero() {
		event.At = time.Now()
	}
	values := map[string]any{
		"type":       string(event.Type),
		"bucket":     event.Bucket.String(),
		"account_id": strconv.FormatInt(event.AccountID, 10),
		"at_unix_ms": strconv.FormatInt(event.At.UnixMilli(), 10),
		"reason":     event.Reason,
		"source":     event.Source,
	}
	return b.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: b.streamKey,
		MaxLen: 100000,
		Approx: true,
		Values: values,
	}).Err()
}

func (b *redisSchedulerEventBus) Subscribe(buffer int) (<-chan service.SchedulerEvent, func()) {
	if b == nil || b.rdb == nil {
		ch := make(chan service.SchedulerEvent)
		close(ch)
		return ch, func() {}
	}
	if buffer <= 0 {
		buffer = 256
	}
	out := make(chan service.SchedulerEvent, buffer)
	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	go b.runSubscriber(ctx, out)
	unsubscribe := func() {
		once.Do(func() {
			cancel()
		})
	}
	return out, unsubscribe
}

func (b *redisSchedulerEventBus) runSubscriber(ctx context.Context, out chan<- service.SchedulerEvent) {
	defer close(out)
	lastID := "$"
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		streams, err := b.rdb.XRead(ctx, &redis.XReadArgs{
			Streams: []string{b.streamKey, lastID},
			Count:   128,
			Block:   time.Second,
		}).Result()
		if err != nil {
			if err == redis.Nil || ctx.Err() != nil {
				continue
			}
			time.Sleep(200 * time.Millisecond)
			continue
		}
		for _, stream := range streams {
			for _, msg := range stream.Messages {
				lastID = msg.ID
				event, ok := parseRedisSchedulerEvent(msg.Values)
				if !ok {
					continue
				}
				select {
				case out <- event:
				default:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

func parseRedisSchedulerEvent(values map[string]any) (service.SchedulerEvent, bool) {
	eventType := service.SchedulerEventType(redisString(values["type"]))
	if eventType == "" {
		return service.SchedulerEvent{}, false
	}
	bucket, _ := service.ParseSchedulerBucket(redisString(values["bucket"]))
	accountID, _ := strconv.ParseInt(redisString(values["account_id"]), 10, 64)
	atUnixMS, _ := strconv.ParseInt(redisString(values["at_unix_ms"]), 10, 64)
	at := time.Time{}
	if atUnixMS > 0 {
		at = time.UnixMilli(atUnixMS)
	}
	return service.SchedulerEvent{
		Type:      eventType,
		Bucket:    bucket,
		AccountID: accountID,
		At:        at,
		Reason:    redisString(values["reason"]),
		Source:    redisString(values["source"]),
	}, true
}

func redisString(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}
