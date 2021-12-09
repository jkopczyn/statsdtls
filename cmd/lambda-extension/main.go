package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"

	"github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"golang.org/x/time/rate"

	"github.com/atlassian/gostatsd"
	"github.com/atlassian/gostatsd/internal/awslambda/extension"
	"github.com/atlassian/gostatsd/internal/util"
	"github.com/atlassian/gostatsd/pkg/backends"
	"github.com/atlassian/gostatsd/pkg/cachedinstances"
	"github.com/atlassian/gostatsd/pkg/cachedinstances/cloudprovider"
	"github.com/atlassian/gostatsd/pkg/cloudproviders"
	"github.com/atlassian/gostatsd/pkg/statsd"
	"github.com/atlassian/gostatsd/pkg/transport"
)

var (
	Version   string
	BuildDate string
	GitCommit string
)

const (
	ParamConfigPath = "config-path"

	ParamVerbose = "verbose"
)

func init() {
	// Allows setting Version at compile time
	// and perserve the value set
	if Version == "" {
		Version = GetVersion()
	}
}

func GetConfiguration() (*viper.Viper, error) {
	v := viper.New()
	util.InitViper(v, "")

	cmd := pflag.NewFlagSet(os.Args[0], pflag.ContinueOnError)
	cmd.Bool(ParamVerbose, false, "Enables debug level logs within the extension")
	cmd.String(ParamConfigPath, "", "Path to a configuration file")

	gostatsd.AddFlags(cmd)

	cmd.VisitAll(func(f *pflag.Flag) {
		if err := v.BindPFlag(f.Name, f); err != nil {
			panic(err)
		}
	})

	if err := cmd.Parse(os.Args[1:]); err != nil {
		return nil, err
	}

	if confPath := v.GetString(ParamConfigPath); confPath != "" {
		v.SetConfigFile(confPath)
		if err := v.ReadInConfig(); err != nil {
			return nil, err
		}
	}

	return v, nil
}

func CreateServer(v *viper.Viper, logger logrus.FieldLogger) (*statsd.Server, error) {
	var runnables []gostatsd.Runnable
	// Logger

	// HTTP client pool
	pool := transport.NewTransportPool(logger, v)

	// Cached instances
	var cachedInstances gostatsd.CachedInstances
	cloudProviderName := v.GetString(gostatsd.ParamCloudProvider)
	if cloudProviderName == "" {
		logger.Info("No cloud provider specified")
	} else {
		var err error
		// See if requested cloud provider is a native CachedInstances implementation
		cachedInstances, err = cachedinstances.Get(logger, cloudProviderName, v, Version)
		switch err {
		case nil:
		case cachedinstances.ErrUnknownProvider:
			// See if requested cloud provider is a CloudProvider implementation
			cloudProvider, err := cloudproviders.Get(logger, cloudProviderName, v, Version)
			if err != nil {
				return nil, err
			}
			runnables = gostatsd.MaybeAppendRunnable(runnables, cloudProvider)
			cachedInstances = newCachedInstancesFromViper(logger, cloudProvider, v)
		default:
			return nil, err
		}
		runnables = gostatsd.MaybeAppendRunnable(runnables, cachedInstances)
	}
	// Backends
	backendNames := v.GetStringSlice(gostatsd.ParamBackends)
	backendsList := make([]gostatsd.Backend, 0, len(backendNames))
	for _, backendName := range backendNames {
		backend, errBackend := backends.InitBackend(backendName, v, logger, pool)
		if errBackend != nil {
			return nil, errBackend
		}
		backendsList = append(backendsList, backend)
		runnables = gostatsd.MaybeAppendRunnable(runnables, backend)
	}
	// Percentiles
	pt, err := getPercentiles(v.GetStringSlice(gostatsd.ParamPercentThreshold))
	if err != nil {
		return nil, err
	}

	// Set defaults for expiry from the main expiry setting
	v.SetDefault(gostatsd.ParamExpiryIntervalCounter, v.GetDuration(gostatsd.ParamExpiryInterval))
	v.SetDefault(gostatsd.ParamExpiryIntervalGauge, v.GetDuration(gostatsd.ParamExpiryInterval))
	v.SetDefault(gostatsd.ParamExpiryIntervalSet, v.GetDuration(gostatsd.ParamExpiryInterval))
	v.SetDefault(gostatsd.ParamExpiryIntervalTimer, v.GetDuration(gostatsd.ParamExpiryInterval))

	// Create server
	return &statsd.Server{
		Runnables:             runnables,
		Backends:              backendsList,
		CachedInstances:       cachedInstances,
		InternalTags:          v.GetStringSlice(gostatsd.ParamInternalTags),
		InternalNamespace:     v.GetString(gostatsd.ParamInternalNamespace),
		DefaultTags:           v.GetStringSlice(gostatsd.ParamDefaultTags),
		Hostname:              gostatsd.Source(v.GetString(gostatsd.ParamHostname)),
		ExpiryIntervalCounter: v.GetDuration(gostatsd.ParamExpiryIntervalCounter),
		ExpiryIntervalGauge:   v.GetDuration(gostatsd.ParamExpiryIntervalGauge),
		ExpiryIntervalSet:     v.GetDuration(gostatsd.ParamExpiryIntervalSet),
		ExpiryIntervalTimer:   v.GetDuration(gostatsd.ParamExpiryIntervalTimer),
		FlushInterval:         v.GetDuration(gostatsd.ParamFlushInterval),
		FlushOffset:           v.GetDuration(gostatsd.ParamFlushOffset),
		FlushAligned:          v.GetBool(gostatsd.ParamFlushAligned),
		IgnoreHost:            v.GetBool(gostatsd.ParamIgnoreHost),
		MaxReaders:            v.GetInt(gostatsd.ParamMaxReaders),
		MaxParsers:            v.GetInt(gostatsd.ParamMaxParsers),
		MaxWorkers:            v.GetInt(gostatsd.ParamMaxWorkers),
		MaxQueueSize:          v.GetInt(gostatsd.ParamMaxQueueSize),
		MaxConcurrentEvents:   v.GetInt(gostatsd.ParamMaxConcurrentEvents),
		EstimatedTags:         v.GetInt(gostatsd.ParamEstimatedTags),
		MetricsAddr:           v.GetString(gostatsd.ParamMetricsAddr),
		Namespace:             v.GetString(gostatsd.ParamNamespace),
		StatserType:           v.GetString(gostatsd.ParamStatserType),
		PercentThreshold:      pt,
		HeartbeatEnabled:      v.GetBool(gostatsd.ParamHeartbeatEnabled),
		ReceiveBatchSize:      v.GetInt(gostatsd.ParamReceiveBatchSize),
		ConnPerReader:         v.GetBool(gostatsd.ParamConnPerReader),
		ServerMode:            v.GetString(gostatsd.ParamServerMode),
		LogRawMetric:          v.GetBool(gostatsd.ParamLogRawMetric),
		HeartbeatTags: gostatsd.Tags{
			fmt.Sprintf("version:%s", Version),
			fmt.Sprintf("commit:%s", GitCommit),
		},
		DisabledSubTypes:          gostatsd.DisabledSubMetrics(v),
		BadLineRateLimitPerSecond: rate.Limit(v.GetFloat64(gostatsd.ParamBadLinesPerMinute) / 60.0),
		HistogramLimit:            v.GetUint32(gostatsd.ParamTimerHistogramLimit),
		Viper:                     v,
		TransportPool:             pool,
	}, nil
}

func getPercentiles(s []string) ([]float64, error) {
	percentThresholds := make([]float64, len(s))
	for i, sPercentThreshold := range s {
		pt, err := strconv.ParseFloat(sPercentThreshold, 64)
		if err != nil {
			return nil, err
		}
		percentThresholds[i] = pt
	}
	return percentThresholds, nil
}

// newCachedInstancesFromViper initialises a new cached instances.
func newCachedInstancesFromViper(logger logrus.FieldLogger, cloudProvider gostatsd.CloudProvider, v *viper.Viper) gostatsd.CachedInstances {
	// Set the defaults in Viper based on the cloud provider values before we manipulate things
	v.SetDefault(gostatsd.ParamCacheRefreshPeriod, gostatsd.DefaultCacheRefreshPeriod)
	v.SetDefault(gostatsd.ParamCacheEvictAfterIdlePeriod, gostatsd.DefaultCacheEvictAfterIdlePeriod)
	v.SetDefault(gostatsd.ParamCacheTTL, gostatsd.DefaultCacheTTL)
	v.SetDefault(gostatsd.ParamCacheNegativeTTL, gostatsd.DefaultCacheNegativeTTL)
	v.SetDefault(gostatsd.ParamMaxCloudRequests, gostatsd.DefaultMaxCloudRequests)
	v.SetDefault(gostatsd.ParamBurstCloudRequests, gostatsd.DefaultBurstCloudRequests)

	// Set the used values based on the defaults merged with any overrides
	cacheOptions := gostatsd.CacheOptions{
		CacheRefreshPeriod:        v.GetDuration(gostatsd.ParamCacheRefreshPeriod),
		CacheEvictAfterIdlePeriod: v.GetDuration(gostatsd.ParamCacheEvictAfterIdlePeriod),
		CacheTTL:                  v.GetDuration(gostatsd.ParamCacheTTL),
		CacheNegativeTTL:          v.GetDuration(gostatsd.ParamCacheNegativeTTL),
	}
	limiter := rate.NewLimiter(rate.Limit(v.GetInt(gostatsd.ParamMaxCloudRequests)), v.GetInt(gostatsd.ParamBurstCloudRequests))
	return cloudprovider.NewCachedCloudProvider(logger, limiter, cloudProvider, cacheOptions)
}

func main() {
	conf, err := GetConfiguration()
	if err != nil {
		panic(err)
	}

	log := logrus.New().WithFields(map[string]interface{}{
		"version":   Version,
		"buildDate": BuildDate,
		"gitCommit": GitCommit,
	})

	log.Logger.SetFormatter(&logrus.JSONFormatter{})

	if conf.GetBool(ParamVerbose) {
		log.Logger.SetLevel(logrus.DebugLevel)
	}

	// TODO: Configure log group

	log.Info("Starting extension runtime")

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	manager, err := extension.NewManager(
		extension.WithLogger(log),
	)
	if err != nil {
		log.WithError(err).Panic("Unable to create extension manager")
	}

	server, err := CreateServer(conf, log)
	if err != nil {
		log.WithError(err).Panic("Unable to create gostatsd server")
	}

	if err := manager.Run(ctx, server); err != nil {
		log.WithError(err).Error("Iss trying to run lambda extension")
	}

	log.Info("Shutting down")
}