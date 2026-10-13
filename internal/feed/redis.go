package feed

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisCache shares ranked feeds across API replicas with explicit TTLs.
type RedisCache struct{ C redis.UniversalClient }

func (r RedisCache) Get(ctx context.Context, k string) ([]byte, bool) {
	b, err := r.C.Get(ctx, k).Bytes()
	return b, err == nil
}

func (r RedisCache) Set(ctx context.Context, k string, v []byte, ttl time.Duration) {
	_ = r.C.Set(ctx, k, v, ttl).Err()
}
