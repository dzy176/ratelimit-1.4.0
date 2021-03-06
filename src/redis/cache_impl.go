package redis

import (
	"bytes"
	"math"
	"math/rand"
	"strconv"
	"sync"
	"time"

	"github.com/coocood/freecache"
	pb_struct "github.com/envoyproxy/go-control-plane/envoy/api/v2/ratelimit"
	pb "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v2"
	"github.com/envoyproxy/ratelimit/src/assert"
	"github.com/envoyproxy/ratelimit/src/config"
	logger "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
)

type rateLimitCacheImpl struct {
	pool Pool
	// Optional Pool for a dedicated cache of per second limits.
	// If this pool is nil, then the Cache will use the pool for all
	// limits regardless of unit. If this pool is not nil, then it
	// is used for limits that have a SECOND unit.
	perSecondPool              Pool
	timeSource                 TimeSource
	jitterRand                 *rand.Rand
	expirationJitterMaxSeconds int64
	// bytes.Buffer pool used to efficiently generate cache keys.
	bufferPool sync.Pool
	localCache *freecache.Cache
}

// Convert a rate limit into a time divider.
// @param unit supplies the unit to convert.
// @return the divider to use in time computations.
func unitToDivider(unit pb.RateLimitResponse_RateLimit_Unit) int64 {
	switch unit {
	case pb.RateLimitResponse_RateLimit_SECOND:
		return 1
	case pb.RateLimitResponse_RateLimit_MINUTE:
		return 60
	case pb.RateLimitResponse_RateLimit_HOUR:
		return 60 * 60
	case pb.RateLimitResponse_RateLimit_DAY:
		return 60 * 60 * 24
	}

	panic("should not get here")
}

// Generate a cache key for a limit lookup.
// @param domain supplies the cache key domain.
// @param descriptor supplies the descriptor to generate the key for.
// @param limit supplies the rate limit to generate the key for (may be nil).
// @param now supplies the current unix time.
// @return cacheKey struct.
func (this *rateLimitCacheImpl) generateCacheKey(
	domain string, descriptor *pb_struct.RateLimitDescriptor, limit *config.RateLimit, now int64) cacheKey {

	if limit == nil {
		return cacheKey{
			key:       "",
			perSecond: false,
		}
	}

	b := this.bufferPool.Get().(*bytes.Buffer)
	defer this.bufferPool.Put(b)
	b.Reset()

	b.WriteString(domain)
	b.WriteByte('_')

	for _, entry := range descriptor.Entries {
		b.WriteString(entry.Key)
		b.WriteByte('_')
		b.WriteString(entry.Value)
		b.WriteByte('_')
	}

	divider := unitToDivider(limit.Limit.Unit)
	b.WriteString(strconv.FormatInt((now/divider)*divider, 10))
	logger.Info("--cacheKey-- ", b.String())
	return cacheKey{
		key:       b.String(),
		perSecond: isPerSecondLimit(limit.Limit.Unit)}
}

func isPerSecondLimit(unit pb.RateLimitResponse_RateLimit_Unit) bool {
	return unit == pb.RateLimitResponse_RateLimit_SECOND
}

func max(a uint32, b uint32) uint32 {
	if a > b {
		return a
	}
	return b
}

type cacheKey struct {
	key string
	// True if the key corresponds to a limit with a SECOND unit. False otherwise.
	perSecond bool
}

func pipelineAppend(conn Connection, key string, hitsAddend uint32, expirationSeconds int64) {
	conn.PipeAppend("INCRBY", key, hitsAddend)
	conn.PipeAppend("EXPIRE", key, expirationSeconds)
}

// append时的 INCRBY, EXPRIRE两次命令, fetch时也应该调用pipResponse()方法
func pipelineFetch(conn Connection) uint32 {
	ret := uint32(conn.PipeResponse().Int())
	// Pop off EXPIRE response and check for error.
	conn.PipeResponse()
	return ret
}

func (this *rateLimitCacheImpl) DoLimit(
	ctx context.Context,
	request *pb.RateLimitRequest,
	limits []*config.RateLimit) []*pb.RateLimitResponse_DescriptorStatus {

	logger.Debugf("starting cache lookup")

	conn := this.pool.Get()
	defer this.pool.Put(conn)

	// Optional connection for per second limits. If the cache has a perSecondPool setup,
	// then use a connection from the pool for per second limits.
	var perSecondConn Connection = nil
	if this.perSecondPool != nil {
		perSecondConn = this.perSecondPool.Get()
		defer this.perSecondPool.Put(perSecondConn)
	}

	// request.HitsAddend could be 0 (default value) if not specified by the caller in the Ratelimit request.
	// 用户可以自定义 命中一次规则时, redis对应key的value增长的步长, 默认是1
	// 这样用户可以在不修改配置文件的前提下调整限速的速率
	// 比如, 默认每条规则限速 10次/秒, 但如果用户自定义步长为2, 那么1秒内访问5次时, token已经达到10, 继续访问则被限制
	// 相当于此时这条限速规则真实速率下降到了 5次/秒
	hitsAddend := max(1, request.HitsAddend)

	// First build a list of all cache keys that we are actually going to hit. generateCacheKey()
	// returns an empty string in the key if there is no limit so that we can keep the arrays
	// all the same size.
	assert.Assert(len(request.Descriptors) == len(limits))
	cacheKeys := make([]cacheKey, len(request.Descriptors))
	now := this.timeSource.UnixNow()
	// 依据request 生成对应的redis key
	// 具体规则时不断拼接key_value, 最后补上一个特殊的时间戳, 形如: test_limit_foo_age_14_1611109560
	for i := 0; i < len(request.Descriptors); i++ {
		cacheKeys[i] = this.generateCacheKey(request.Domain, request.Descriptors[i], limits[i], now)

		// Increase statistics for limits hit by their respective requests.
		if limits[i] != nil {
			limits[i].Stats.TotalHits.Add(uint64(hitsAddend))
		}
	}

	isOverLimitWithLocalCache := make([]bool, len(request.Descriptors))

	// Now, actually setup the pipeline, skipping empty cache keys.
	for i, cacheKey := range cacheKeys {
		if cacheKey.key == "" {
			continue
		}

		if this.localCache != nil {
			// Get returns the value or not found error.
			_, err := this.localCache.Get([]byte(cacheKey.key))
			if err == nil {
				isOverLimitWithLocalCache[i] = true
				logger.Debugf("cache key is over the limit: %s", cacheKey.key)
				continue
			}
		}

		logger.Debugf("looking up cache key: %s", cacheKey.key)

		expirationSeconds := unitToDivider(limits[i].Limit.Unit)
		if this.expirationJitterMaxSeconds > 0 {
			expirationSeconds += this.jitterRand.Int63n(this.expirationJitterMaxSeconds)
		}

		// 与redis交互
		// pipeline可以减少与redis通信的次数
		// Use the perSecondConn if it is not nil and the cacheKey represents a per second Limit.
		if perSecondConn != nil && cacheKey.perSecond {
			pipelineAppend(perSecondConn, cacheKey.key, hitsAddend, expirationSeconds)
		} else {
			pipelineAppend(conn, cacheKey.key, hitsAddend, expirationSeconds)
		}
	}

	// Now fetch the pipeline.
	responseDescriptorStatuses := make([]*pb.RateLimitResponse_DescriptorStatus,
		len(request.Descriptors))
	for i, cacheKey := range cacheKeys {
		// fast path: 这类request没有命中规则, 无需redis计数统计, 返回ok
		if cacheKey.key == "" {
			responseDescriptorStatuses[i] =
				&pb.RateLimitResponse_DescriptorStatus{
					Code:           pb.RateLimitResponse_OK,
					CurrentLimit:   nil,
					LimitRemaining: 0,
				}
			continue
		}

		if isOverLimitWithLocalCache[i] {
			responseDescriptorStatuses[i] =
				&pb.RateLimitResponse_DescriptorStatus{
					Code:           pb.RateLimitResponse_OVER_LIMIT,
					CurrentLimit:   limits[i].Limit,
					LimitRemaining: 0,
				}
			limits[i].Stats.OverLimit.Add(uint64(hitsAddend))
			limits[i].Stats.OverLimitWithLocalCache.Add(uint64(hitsAddend))
			continue
		}

		var limitAfterIncrease uint32
		// Use the perSecondConn if it is not nil and the cacheKey represents a per second Limit.
		if this.perSecondPool != nil && cacheKey.perSecond {
			limitAfterIncrease = pipelineFetch(perSecondConn)
		} else {
			limitAfterIncrease = pipelineFetch(conn)
		}

		// 操作redis前
		limitBeforeIncrease := limitAfterIncrease - hitsAddend
		// 阈值
		overLimitThreshold := limits[i].Limit.RequestsPerUnit
		// The nearLimitThreshold is the number of requests that can be made before hitting the NearLimitRatio.
		// We need to know it in both the OK and OVER_LIMIT scenarios.
		// math.Floor: 四舍五入; nearLimitThreshold的作用暂时不太能理解
		nearLimitThreshold := uint32(math.Floor(float64(float32(overLimitThreshold) * config.NearLimitRatio)))

		logger.Debugf("cache key: %s current: %d", cacheKey.key, limitAfterIncrease)
		if limitAfterIncrease > overLimitThreshold {
			responseDescriptorStatuses[i] =
				&pb.RateLimitResponse_DescriptorStatus{
					Code:           pb.RateLimitResponse_OVER_LIMIT,
					CurrentLimit:   limits[i].Limit,
					LimitRemaining: 0,
				}

			// Increase over limit statistics. Because we support += behavior for increasing the limit, we need to
			// assess if the entire hitsAddend were over the limit. That is, if the limit's value before adding the
			// N hits was over the limit, then all the N hits were over limit.
			// Otherwise, only the difference between the current limit value and the over limit threshold
			// were over limit hits.
			if limitBeforeIncrease >= overLimitThreshold {
				limits[i].Stats.OverLimit.Add(uint64(hitsAddend))
			} else {
				limits[i].Stats.OverLimit.Add(uint64(limitAfterIncrease - overLimitThreshold))

				// If the limit before increase was below the over limit value, then some of the hits were
				// in the near limit range.
				limits[i].Stats.NearLimit.Add(uint64(overLimitThreshold - max(nearLimitThreshold, limitBeforeIncrease)))
			}
			if this.localCache != nil {
				// Set the TTL of the local_cache to be the entire duration.
				// Since the cache_key gets changed once the time crosses over current time slot, the over-the-limit
				// cache keys in local_cache lose effectiveness.
				// For example, if we have an hour limit on all mongo connections, the cache key would be
				// similar to mongo_1h, mongo_2h, etc. In the hour 1 (0h0m - 0h59m), the cache key is mongo_1h, we start
				// to get ratelimited in the 50th minute, the ttl of local_cache will be set as 1 hour(0h50m-1h49m).
				// In the time of 1h1m, since the cache key becomes different (mongo_2h), it won't get ratelimited.
				err := this.localCache.Set([]byte(cacheKey.key), []byte{}, int(unitToDivider(limits[i].Limit.Unit)))
				if err != nil {
					logger.Errorf("Failing to set local cache key: %s", cacheKey.key)
				}
			}
		} else {
			responseDescriptorStatuses[i] =
				&pb.RateLimitResponse_DescriptorStatus{
					Code:           pb.RateLimitResponse_OK,
					CurrentLimit:   limits[i].Limit,
					LimitRemaining: overLimitThreshold - limitAfterIncrease,
				}

			// The limit is OK but we additionally want to know if we are near the limit.
			if limitAfterIncrease > nearLimitThreshold {
				// Here we also need to assess which portion of the hitsAddend were in the near limit range.
				// If all the hits were over the nearLimitThreshold, then all hits are near limit. Otherwise,
				// only the difference between the current limit value and the near limit threshold were near
				// limit hits.
				if limitBeforeIncrease >= nearLimitThreshold {
					limits[i].Stats.NearLimit.Add(uint64(hitsAddend))
				} else {
					limits[i].Stats.NearLimit.Add(uint64(limitAfterIncrease - nearLimitThreshold))
				}
			}
		}
	}

	return responseDescriptorStatuses
}

func NewRateLimitCacheImpl(pool Pool, perSecondPool Pool, timeSource TimeSource, jitterRand *rand.Rand, expirationJitterMaxSeconds int64, localCache *freecache.Cache) RateLimitCache {
	return &rateLimitCacheImpl{
		pool:                       pool,
		perSecondPool:              perSecondPool,
		timeSource:                 timeSource,
		jitterRand:                 jitterRand,
		expirationJitterMaxSeconds: expirationJitterMaxSeconds,
		bufferPool:                 newBufferPool(),
		localCache:                 localCache,
	}
}

func newBufferPool() sync.Pool {
	return sync.Pool{
		New: func() interface{} {
			return new(bytes.Buffer)
		},
	}
}

type timeSourceImpl struct{}

func NewTimeSourceImpl() TimeSource {
	return &timeSourceImpl{}
}

func (this *timeSourceImpl) UnixNow() int64 {
	return time.Now().Unix()
}

// rand for jitter.
type lockedSource struct {
	lk  sync.Mutex
	src rand.Source
}

func NewLockedSource(seed int64) JitterRandSource {
	return &lockedSource{src: rand.NewSource(seed)}
}

func (r *lockedSource) Int63() (n int64) {
	r.lk.Lock()
	n = r.src.Int63()
	r.lk.Unlock()
	return
}

func (r *lockedSource) Seed(seed int64) {
	r.lk.Lock()
	r.src.Seed(seed)
	r.lk.Unlock()
}
