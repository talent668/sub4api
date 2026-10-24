package repository

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestWSFallbackReceiptAtomicScopeExpiryAndCapacity(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer func() { _ = client.Close() }()
	cache := &gatewayCache{rdb: client}
	ctx := context.Background()
	hash, binding, id := strings.Repeat("a", 64), strings.Repeat("b", 64), uuid.NewString()
	ok, err := cache.PutWSFallbackReceipt(ctx, 11, hash, binding, id)
	require.NoError(t, err)
	require.True(t, ok)
	wrong, err := cache.ConsumeWSFallbackReceipt(ctx, 12, hash, binding)
	require.NoError(t, err)
	require.Empty(t, wrong)
	wrong, err = cache.ConsumeWSFallbackReceipt(ctx, 11, hash, strings.Repeat("c", 64))
	require.NoError(t, err)
	require.Empty(t, wrong)

	var hits atomic.Int64
	var workers sync.WaitGroup
	for range 12 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			value, consumeErr := cache.ConsumeWSFallbackReceipt(ctx, 11, hash, binding)
			if consumeErr != nil {
				t.Error(consumeErr)
			}
			if value == id {
				hits.Add(1)
			} else if value != "" {
				t.Errorf("unexpected failure ID: %q", value)
			}
		}()
	}
	workers.Wait()
	require.EqualValues(t, 1, hits.Load())

	for i := range 32 {
		ok, err = cache.PutWSFallbackReceipt(ctx, 11, fmt.Sprintf("%064x", i), binding, uuid.NewString())
		require.NoError(t, err)
		require.True(t, ok)
	}
	ok, err = cache.PutWSFallbackReceipt(ctx, 11, hash, binding, id)
	require.NoError(t, err)
	require.False(t, ok, "each key must have bounded observation storage")
	count, err := client.HLen(ctx, wsFallbackCacheKey(11)).Result()
	require.NoError(t, err)
	require.EqualValues(t, 32, count)
	server.FastForward(121 * time.Second)
	value, err := cache.ConsumeWSFallbackReceipt(ctx, 11, strings.Repeat("0", 64), binding)
	require.NoError(t, err)
	require.Empty(t, value)
	ok, err = cache.PutWSFallbackReceipt(ctx, 11, hash, binding, id)
	require.NoError(t, err)
	require.True(t, ok)
	value, err = cache.ConsumeWSFallbackReceipt(ctx, 11, hash, binding)
	require.NoError(t, err)
	require.Equal(t, id, value)
}

func TestWSFallbackReceiptExpiresWithinActiveHash(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer func() { _ = client.Close() }()
	cache := &gatewayCache{rdb: client}
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	server.SetTime(now)
	binding := strings.Repeat("b", 64)
	first, second := strings.Repeat("a", 64), strings.Repeat("c", 64)
	_, err := cache.PutWSFallbackReceipt(ctx, 11, first, binding, uuid.NewString())
	require.NoError(t, err)
	server.SetTime(now.Add(90 * time.Second))
	_, err = cache.PutWSFallbackReceipt(ctx, 11, second, binding, uuid.NewString())
	require.NoError(t, err)
	server.SetTime(now.Add(121 * time.Second))
	value, err := cache.ConsumeWSFallbackReceipt(ctx, 11, first, binding)
	require.NoError(t, err)
	require.Empty(t, value, "refreshing the hash TTL must not extend an old receipt")
	value, err = cache.ConsumeWSFallbackReceipt(ctx, 11, second, binding)
	require.NoError(t, err)
	require.NotEmpty(t, value)
}

func TestWSFallbackReceiptSlowRedisHasBoundedIO(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		<-stop // Accept the TCP connection but never answer Redis commands.
	}()
	client := redis.NewClient(&redis.Options{Addr: listener.Addr().String(), ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second, MaxRetries: -1, PoolSize: 1})
	defer func() { _ = client.Close() }()
	cache := &gatewayCache{rdb: client}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = cache.PutWSFallbackReceipt(ctx, 11, strings.Repeat("a", 64), strings.Repeat("b", 64), uuid.NewString())
	require.Error(t, err)
	require.Less(t, time.Since(started), 750*time.Millisecond, "optional observations must not inherit a multi-second socket timeout")
	require.Equal(t, 2*time.Second, client.Options().ReadTimeout, "the main Redis client's settings must remain unchanged")
}
