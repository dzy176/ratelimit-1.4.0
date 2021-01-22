package ratelimit

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	pb "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v2"
	"github.com/envoyproxy/ratelimit/src/assert"
	"github.com/envoyproxy/ratelimit/src/config"
	"github.com/envoyproxy/ratelimit/src/redis"
	"github.com/lyft/goruntime/loader"
	"github.com/lyft/gostats"
	logger "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
)

type shouldRateLimitStats struct {
	redisError   stats.Counter
	serviceError stats.Counter
}

func newShouldRateLimitStats(scope stats.Scope) shouldRateLimitStats {
	ret := shouldRateLimitStats{}
	ret.redisError = scope.NewCounter("redis_error")
	ret.serviceError = scope.NewCounter("service_error")
	return ret
}

type serviceStats struct {
	configLoadSuccess stats.Counter
	configLoadError   stats.Counter
	shouldRateLimit   shouldRateLimitStats
}

func newServiceStats(scope stats.Scope) serviceStats {
	ret := serviceStats{}
	ret.configLoadSuccess = scope.NewCounter("config_load_success")
	ret.configLoadError = scope.NewCounter("config_load_error")
	ret.shouldRateLimit = newShouldRateLimitStats(scope.Scope("call.should_rate_limit"))
	return ret
}

type RateLimitServiceServer interface {
	pb.RateLimitServiceServer
	GetCurrentConfig() config.RateLimitConfig
	GetLegacyService() RateLimitLegacyServiceServer
}

type service struct {
	runtime            loader.IFace // 加载配置文件
	configLock         sync.RWMutex
	configLoader       config.RateLimitConfigLoader // 反序列化配置文件
	config             config.RateLimitConfig       // dump配置; 依据是否命中, 获取相应配置
	runtimeUpdateEvent chan int                     //　配置文件更新信号
	cache              redis.RateLimitCache         // redis
	stats              serviceStats
	rlStatsScope       stats.Scope
	legacy             *legacyService
}

func (this *service) reloadConfig() {
	defer func() {
		if e := recover(); e != nil {
			configError, ok := e.(config.RateLimitConfigError)
			if !ok {
				panic(e)
			}

			this.stats.configLoadError.Inc()
			logger.Errorf("error loading new configuration from runtime: %s", configError.Error())
		}
	}()

	files := []config.RateLimitConfigToLoad{}
	snapshot := this.runtime.Snapshot()
	for _, key := range snapshot.Keys() {
		// 原文中config必须是config开头的, 不合理, 注释掉
		//if !strings.HasPrefix(key, "config.") {
		//	continue
		//}

		files = append(files, config.RateLimitConfigToLoad{key, snapshot.Get(key)})
	}

	newConfig := this.configLoader.Load(files, this.rlStatsScope)
	this.stats.configLoadSuccess.Inc()
	this.configLock.Lock()
	this.config = newConfig
	this.configLock.Unlock()
}

type serviceError string

func (e serviceError) Error() string {
	return string(e)
}

func checkServiceErr(something bool, msg string) {
	if !something {
		panic(serviceError(msg))
	}
}

func (this *service) shouldRateLimitWorker(ctx context.Context, request *pb.RateLimitRequest) *pb.RateLimitResponse {

	checkServiceErr(request.Domain != "", "rate limit domain must not be empty")
	checkServiceErr(len(request.Descriptors) != 0, "rate limit descriptor list must not be empty")

	// 加载限速配置
	snappedConfig := this.GetCurrentConfig()
	checkServiceErr(snappedConfig != nil, "no rate limit configuration loaded")

	// limitToCheck的长度对应了传入request.descriptors中待判定的条目长度;
	// 遍历request与限速配置进行关键字match, 命中对应数组位置初始化一个*config.RateLimit, 之后与redis交互, 判定是否429
	// 没有匹配则将对应位置初始化为nil, 此时该下标位置的request不会与后端redis交互, 直接判定ok
	limitsToCheck := make([]*config.RateLimit, len(request.Descriptors))
	for i, descriptor := range request.Descriptors {

		// debug start
		var descriptorEntryStrings []string
		for _, descriptorEntry := range descriptor.GetEntries() {
			descriptorEntryStrings = append(
				descriptorEntryStrings,
				fmt.Sprintf("(%s=%s)", descriptorEntry.Key, descriptorEntry.Value),
			)
		}
		logger.Infof("got descriptor from request: %s", strings.Join(descriptorEntryStrings, ","))
		// debug end

		limitsToCheck[i] = snappedConfig.GetLimit(ctx, request.Domain, descriptor)

		lB, _ := json.MarshalIndent(limitsToCheck[i], "  ", " ")
		logger.Infof("limit to check [%d]:\n%s", i, string(lB))
	}

	responseDescriptorStatuses := this.cache.DoLimit(ctx, request, limitsToCheck)
	assert.Assert(len(limitsToCheck) == len(responseDescriptorStatuses))

	response := &pb.RateLimitResponse{}
	response.Statuses = make([]*pb.RateLimitResponse_DescriptorStatus, len(request.Descriptors))
	// 传入多个descriptos时, 全部不被限速, overall code = ok
	finalCode := pb.RateLimitResponse_OK
	for i, descriptorStatus := range responseDescriptorStatuses {
		response.Statuses[i] = descriptorStatus
		if descriptorStatus.Code == pb.RateLimitResponse_OVER_LIMIT {
			finalCode = descriptorStatus.Code
		}
	}

	response.OverallCode = finalCode
	return response
}

func (this *service) ShouldRateLimit(ctx context.Context, request *pb.RateLimitRequest) (finalResponse *pb.RateLimitResponse, finalError error) {
	// grpc func 入口
	logger.Info("**** enter ****")
	defer func() {
		err := recover()
		if err == nil {
			return
		}

		logger.Infof("caught error during call")
		finalResponse = nil
		switch t := err.(type) {
		case redis.RedisError:
			{
				this.stats.shouldRateLimit.redisError.Inc()
				finalError = t
			}
		case serviceError:
			{
				this.stats.shouldRateLimit.serviceError.Inc()
				finalError = t
			}
		default:
			panic(err)
		}
	}()

	response := this.shouldRateLimitWorker(ctx, request)
	logger.Debugf("returning normal response")
	return response, nil
}

func (this *service) GetLegacyService() RateLimitLegacyServiceServer {
	return this.legacy
}

func (this *service) GetCurrentConfig() config.RateLimitConfig {
	this.configLock.RLock()
	defer this.configLock.RUnlock()
	return this.config
}

func NewService(runtime loader.IFace, cache redis.RateLimitCache,
	configLoader config.RateLimitConfigLoader, stats stats.Scope) RateLimitServiceServer {

	newService := &service{
		runtime:            runtime,
		configLock:         sync.RWMutex{},
		configLoader:       configLoader,
		config:             nil,
		runtimeUpdateEvent: make(chan int),
		cache:              cache,
		stats:              newServiceStats(stats),
		rlStatsScope:       stats.Scope("rate_limit"),
	}
	newService.legacy = &legacyService{
		s:                          newService,
		shouldRateLimitLegacyStats: newShouldRateLimitLegacyStats(stats),
	}

	runtime.AddUpdateCallback(newService.runtimeUpdateEvent)

	newService.reloadConfig()
	go func() {
		// No exit right now.
		for {
			logger.Debugf("waiting for runtime update")
			<-newService.runtimeUpdateEvent
			logger.Debugf("got runtime update and reloading config")
			newService.reloadConfig()
		}
	}()

	return newService
}
