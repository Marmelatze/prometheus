package promserv

import (
	"flag"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/log"
	"github.com/prometheus/prometheus/notification"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/storage/local"
	"github.com/prometheus/prometheus/storage/local/index"
	"github.com/prometheus/prometheus/web"
)

// config is the immutable configuration for a running Prometheus server.
type conf struct {
	fs *flag.FlagSet

	printVersion bool
	configFile   string

	storage      local.MemorySeriesStorageOptions
	notification notification.NotificationHandlerOptions
	queryEngine  promql.EngineOptions
	web          web.Options

	// Remote storage.
	remoteStorageTimeout  time.Duration
	influxURL             string
	influxRetentionPolicy string
	influxDatabase        string
	opentsdbURL           string

	prometheusURL string
}

var cfg = &conf{}

func init() {
	cfg.fs = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	cfg.fs.Usage = cfg.usage

	cfg.fs.BoolVar(
		&cfg.printVersion, "version", false,
		"Print version information.",
	)
	cfg.fs.StringVar(
		&cfg.configFile, "config.file", "prometheus.yml",
		"Prometheus configuration file name.",
	)

	// Web.
	cfg.fs.StringVar(
		&cfg.web.PathPrefix, "web.path-prefix", "",
		"Prefix for all web paths.",
	)
	cfg.fs.StringVar(
		&cfg.web.ListenAddress, "web.listen-address", ":9090",
		"Address to listen on for the web interface, API, and telemetry.",
	)
	cfg.fs.StringVar(
		&cfg.web.Hostname, "web.hostname", "",
		"Hostname on which the server is available.",
	)
	cfg.fs.StringVar(
		&cfg.web.MetricsPath, "web.telemetry-path", "/metrics",
		"Path under which to expose metrics.",
	)
	cfg.fs.BoolVar(
		&cfg.web.UseLocalAssets, "web.use-local-assets", false,
		"Read assets/templates from file instead of binary.",
	)
	cfg.fs.StringVar(
		&cfg.web.UserAssetsPath, "web.user-assets", "",
		"Path to static asset directory, available at /user.",
	)
	cfg.fs.BoolVar(
		&cfg.web.EnableQuit, "web.enable-remote-shutdown", false,
		"Enable remote service shutdown.",
	)
	cfg.fs.StringVar(
		&cfg.web.ConsoleTemplatesPath, "web.console.templates", "consoles",
		"Path to the console template directory, available at /console.",
	)
	cfg.fs.StringVar(
		&cfg.web.ConsoleLibrariesPath, "web.console.libraries", "console_libraries",
		"Path to the console library directory.",
	)

	// Storage.
	cfg.fs.StringVar(
		&cfg.storage.PersistenceStoragePath, "storage.local.path", "data",
		"Base path for metrics storage.",
	)
	cfg.fs.IntVar(
		&cfg.storage.MemoryChunks, "storage.local.memory-chunks", 1024*1024,
		"How many chunks to keep in memory. While the size of a chunk is 1kiB, the total memory usage will be significantly higher than this value * 1kiB. Furthermore, for various reasons, more chunks might have to be kept in memory temporarily.",
	)
	cfg.fs.DurationVar(
		&cfg.storage.PersistenceRetentionPeriod, "storage.local.retention", 15*24*time.Hour,
		"How long to retain samples in the local storage.",
	)
	cfg.fs.IntVar(
		&cfg.storage.MaxChunksToPersist, "storage.local.max-chunks-to-persist", 1024*1024,
		"How many chunks can be waiting for persistence before sample ingestion will stop. Many chunks waiting to be persisted will increase the checkpoint size.",
	)
	cfg.fs.DurationVar(
		&cfg.storage.CheckpointInterval, "storage.local.checkpoint-interval", 5*time.Minute,
		"The period at which the in-memory metrics and the chunks not yet persisted to series files are checkpointed.",
	)
	cfg.fs.IntVar(
		&cfg.storage.CheckpointDirtySeriesLimit, "storage.local.checkpoint-dirty-series-limit", 5000,
		"If approx. that many time series are in a state that would require a recovery operation after a crash, a checkpoint is triggered, even if the checkpoint interval hasn't passed yet. A recovery operation requires a disk seek. The default limit intends to keep the recovery time below 1min even on spinning disks. With SSD, recovery is much faster, so you might want to increase this value in that case to avoid overly frequent checkpoints.",
	)
	cfg.fs.Var(
		&cfg.storage.SyncStrategy, "storage.local.series-sync-strategy",
		"When to sync series files after modification. Possible values: 'never', 'always', 'adaptive'. Sync'ing slows down storage performance but reduces the risk of data loss in case of an OS crash. With the 'adaptive' strategy, series files are sync'd for as long as the storage is not too much behind on chunk persistence.",
	)
	cfg.fs.BoolVar(
		&cfg.storage.Dirty, "storage.local.dirty", false,
		"If set, the local storage layer will perform crash recovery even if the last shutdown appears to be clean.",
	)
	cfg.fs.BoolVar(
		&cfg.storage.PedanticChecks, "storage.local.pedantic-checks", false,
		"If set, a crash recovery will perform checks on each series file. This might take a very long time.",
	)
	cfg.fs.Var(
		&local.DefaultChunkEncoding, "storage.local.chunk-encoding-version",
		"Which chunk encoding version to use for newly created chunks. Currently supported is 0 (delta encoding) and 1 (double-delta encoding).",
	)
	// Index cache sizes.
	cfg.fs.IntVar(
		&index.FingerprintMetricCacheSize, "storage.local.index-cache-size.fingerprint-to-metric", index.FingerprintMetricCacheSize,
		"The size in bytes for the fingerprint to metric index cache.",
	)
	cfg.fs.IntVar(
		&index.FingerprintTimeRangeCacheSize, "storage.local.index-cache-size.fingerprint-to-timerange", index.FingerprintTimeRangeCacheSize,
		"The size in bytes for the metric time range index cache.",
	)
	cfg.fs.IntVar(
		&index.LabelNameLabelValuesCacheSize, "storage.local.index-cache-size.label-name-to-label-values", index.LabelNameLabelValuesCacheSize,
		"The size in bytes for the label name to label values index cache.",
	)
	cfg.fs.IntVar(
		&index.LabelPairFingerprintsCacheSize, "storage.local.index-cache-size.label-pair-to-fingerprints", index.LabelPairFingerprintsCacheSize,
		"The size in bytes for the label pair to fingerprints index cache.",
	)

	// Remote storage.
	cfg.fs.StringVar(
		&cfg.opentsdbURL, "storage.remote.opentsdb-url", "",
		"The URL of the remote OpenTSDB server to send samples to. None, if empty.",
	)
	cfg.fs.StringVar(
		&cfg.influxURL, "storage.remote.influxdb-url", "",
		"The URL of the remote InfluxDB server to send samples to. None, if empty.",
	)
	cfg.fs.StringVar(
		&cfg.influxRetentionPolicy, "storage.remote.influxdb.retention-policy", "default",
		"The InfluxDB retention policy to use.",
	)
	cfg.fs.StringVar(
		&cfg.influxDatabase, "storage.remote.influxdb.database", "prometheus",
		"The name of the database to use for storing samples in InfluxDB.",
	)
	cfg.fs.DurationVar(
		&cfg.remoteStorageTimeout, "storage.remote.timeout", 30*time.Second,
		"The timeout to use when sending samples to the remote storage.",
	)

	// Alertmanager.
	cfg.fs.StringVar(
		&cfg.notification.AlertmanagerURL, "alertmanager.url", "",
		"The URL of the alert manager to send notifications to.",
	)
	cfg.fs.IntVar(
		&cfg.notification.QueueCapacity, "alertmanager.notification-queue-capacity", 100,
		"The capacity of the queue for pending alert manager notifications.",
	)
	cfg.fs.DurationVar(
		&cfg.notification.Deadline, "alertmanager.http-deadline", 10*time.Second,
		"Alert manager HTTP API timeout.",
	)

	// Query engine.
	cfg.fs.DurationVar(
		&promql.StalenessDelta, "query.staleness-delta", promql.StalenessDelta,
		"Staleness delta allowance during expression evaluations.",
	)
	cfg.fs.DurationVar(
		&cfg.queryEngine.Timeout, "query.timeout", 2*time.Minute,
		"Maximum time a query may take before being aborted.",
	)
	cfg.fs.IntVar(
		&cfg.queryEngine.MaxConcurrentQueries, "query.max-concurrency", 20,
		"Maximum number of queries executed concurrently.",
	)

	// Set additional defaults.
	cfg.storage.SyncStrategy = local.Adaptive
}

func (c *conf) parse(args []string) error {
	err := c.fs.Parse(args)
	if err != nil {
		if err != flag.ErrHelp {
			log.Errorf("Invalid command line arguments. Help: %s -h", os.Args[0])
		}
		return err
	}

	c.web.PathPrefix = strings.TrimRight(c.web.PathPrefix, "/")
	if c.web.PathPrefix != "" && !strings.HasPrefix(c.web.PathPrefix, "/") {
		c.web.PathPrefix = "/" + c.web.PathPrefix
	}

	if c.web.Hostname == "" {
		c.web.Hostname, err = os.Hostname()
		if err != nil {
			return err
		}
	}

	_, port, err := net.SplitHostPort(c.web.ListenAddress)
	if err != nil {
		return err
	}
	c.prometheusURL = fmt.Sprintf("http://%s:%s%s/", cfg.web.Hostname, port, c.web.PathPrefix)

	return nil
}

func (c *conf) usage() {
	groups := make(map[string][]*flag.Flag)
	// Set a default group for ungrouped flags.
	groups["."] = make([]*flag.Flag, 0)

	// Bucket flags into groups based on the first of their dot-separated levels.
	c.fs.VisitAll(func(fl *flag.Flag) {
		parts := strings.SplitN(fl.Name, ".", 2)
		if len(parts) == 1 {
			groups["."] = append(groups["."], fl)
		} else {
			name := parts[0]
			groups[name] = append(groups[name], fl)
		}
	})

	groupsOrdered := make(sort.StringSlice, 0, len(groups))
	for groupName := range groups {
		groupsOrdered = append(groupsOrdered, groupName)
	}
	sort.Sort(groupsOrdered)

	fmt.Fprintf(os.Stderr, "Usage: %s [options ...]:\n\n", os.Args[0])

	const (
		maxLineLength = 80
		lineSep       = "\n      "
	)
	for _, groupName := range groupsOrdered {
		if groupName != "." {
			fmt.Fprintf(os.Stderr, "\n%s:\n", strings.Title(groupName))
		}

		for _, fl := range groups[groupName] {
			format := "  -%s=%s"
			if strings.Contains(fl.DefValue, " ") || fl.DefValue == "" {
				format = "  -%s=%q"
			}
			flagUsage := fmt.Sprintf(format+lineSep, fl.Name, fl.DefValue)

			// Format the usage text to not exceed maxLineLength characters per line.
			words := strings.SplitAfter(fl.Usage, " ")
			lineLength := len(lineSep) - 1
			for _, w := range words {
				if lineLength+len(w) > maxLineLength {
					flagUsage += lineSep
					lineLength = len(lineSep) - 1
				}
				flagUsage += w
				lineLength += len(w)
			}
			fmt.Fprintf(os.Stderr, "%s\n", flagUsage)
		}
	}
}
