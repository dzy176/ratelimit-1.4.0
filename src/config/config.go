package config

import (
	pb_struct "github.com/envoyproxy/go-control-plane/envoy/api/v2/ratelimit"
	pb "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v2"
	stats "github.com/lyft/gostats"
	"golang.org/x/net/context"
)

// The NearLimitRation constant defines the ratio of total_hits over
// the Limit's RequestPerUnit that need to happen before triggering a near_limit
// stat increase
const NearLimitRatio = 0.8

// Errors that may be raised during config parsing.
type RateLimitConfigError string

func (e RateLimitConfigError) Error() string {
	return string(e)
}

// Stats for an individual rate Limit config entry.
type RateLimitStats struct {
	TotalHits               stats.Counter
	OverLimit               stats.Counter
	NearLimit               stats.Counter
	OverLimitWithLocalCache stats.Counter
}

// Wrapper for an individual rate Limit config entry which includes the defined Limit and stats.
type RateLimit struct {
	FullKey string `json:"full_key"`
	Stats   RateLimitStats `json:"stats"`
	Limit   *pb.RateLimitResponse_RateLimit `json:"Limit"`
}

// Interface for interacting with a loaded rate Limit config.
type RateLimitConfig interface {
	// Dump the configuration into string form for debugging.
	Dump() string

	// Get the configured Limit for a rate Limit descriptor.
	// @param ctx supplies the calling context.
	// @param domain supplies the domain to lookup the descriptor in.
	// @param descriptor supplies the descriptor to look up.
	// @return a rate Limit to apply or nil if no rate Limit is configured for the descriptor.
	GetLimit(ctx context.Context, domain string, descriptor *pb_struct.RateLimitDescriptor) *RateLimit
}

// Information for a config file to load into the aggregate config.
type RateLimitConfigToLoad struct {
	Name      string
	FileBytes string
}

// Interface for loading a configuration from a list of YAML files.
type RateLimitConfigLoader interface {
	// Load a new configuration from a list of YAML files.
	// @param configs supplies a list of full YAML files in string form.
	// @param statsScope supplies the stats scope to use for Limit stats during runtime.
	// @return a new configuration.
	// @throws RateLimitConfigError if the configuration could not be created.
	Load(configs []RateLimitConfigToLoad, statsScope stats.Scope) RateLimitConfig
}
