// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Tetragon
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cilium/tetragon/api/v1/tetragon"
	"github.com/cilium/tetragon/pkg/bpf"
	"github.com/cilium/tetragon/pkg/btf"
	"github.com/cilium/tetragon/pkg/bugtool"
	"github.com/cilium/tetragon/pkg/cilium"
	"github.com/cilium/tetragon/pkg/config"
	"github.com/cilium/tetragon/pkg/defaults"
	"github.com/cilium/tetragon/pkg/exporter"
	"github.com/cilium/tetragon/pkg/filters"
	tetragonGrpc "github.com/cilium/tetragon/pkg/grpc"
	"github.com/cilium/tetragon/pkg/logger"
	"github.com/cilium/tetragon/pkg/metrics"
	"github.com/cilium/tetragon/pkg/observer"
	"github.com/cilium/tetragon/pkg/option"
	"github.com/cilium/tetragon/pkg/process"
	"github.com/cilium/tetragon/pkg/ratelimit"
	"github.com/cilium/tetragon/pkg/sensors"
	"github.com/cilium/tetragon/pkg/sensors/base"
	"github.com/cilium/tetragon/pkg/server"
	"github.com/cilium/tetragon/pkg/version"
	"github.com/cilium/tetragon/pkg/watcher"
	"github.com/cilium/tetragon/pkg/watcher/crd"

	// Imported to allow sensors to be initialized inside init().
	_ "github.com/cilium/tetragon/pkg/sensors"

	"github.com/cilium/lumberjack/v2"
	gops "github.com/google/gops/agent"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/durationpb"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var (
	log = logger.GetLogger()
)

func getExportFilters() ([]*tetragon.Filter, []*tetragon.Filter, error) {
	allowList, err := filters.ParseFilterList(viper.GetString(keyExportAllowlist))
	if err != nil {
		return nil, nil, err
	}
	denyList, err := filters.ParseFilterList(viper.GetString(keyExportDenylist))
	if err != nil {
		return nil, nil, err
	}
	return allowList, denyList, nil
}

func saveInitInfo() error {
	info := bugtool.InitInfo{
		ExportFname: exportFilename,
		LibDir:      option.Config.HubbleLib,
		BtfFname:    option.Config.BTF,
		MetricsAddr: metricsServer,
		ServerAddr:  serverAddress,
	}
	return bugtool.SaveInitInfo(&info)
}

func readConfig(file string) (*config.GenericTracingConf, error) {
	if file == "" {
		return nil, nil
	}

	yamlData, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("failed to read yaml file %s: %w", configFile, err)
	}
	cnf, err := config.ReadConfigYaml(string(yamlData))
	if err != nil {
		return nil, err
	}

	return cnf, nil
}

func stopProfile() {
	if memProfile != "" {
		log.WithField("file", memProfile).Info("Stopping mem profiling")
		f, err := os.Create(memProfile)
		if err != nil {
			log.WithField("file", memProfile).Fatal("Could not create memory profile: ", err)
		}
		defer f.Close()
		// get up-to-date statistics
		runtime.GC()
		if err := pprof.WriteHeapProfile(f); err != nil {
			log.Fatal("could not write memory profile: ", err)
		}
	}
	if cpuProfile != "" {
		log.WithField("file", cpuProfile).Info("Stopping cpu profiling")
		pprof.StopCPUProfile()
	}
}

func tetragonExecute() error {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	readAndSetFlags()

	// Logging should always be bootstrapped first. Do not add any code above this!
	if err := logger.SetupLogging(option.Config.LogOpts, option.Config.Debug); err != nil {
		log.Fatal(err)
	}

	log.WithField("version", version.Version).Info("Starting tetragon")
	log.WithField("config", viper.AllSettings()).Info("config settings")

	if viper.IsSet(keyNetnsDir) {
		defaults.NetnsDir = viper.GetString(keyNetnsDir)
	}

	if memProfile != "" {
		log.WithField("file", memProfile).Info("Starting mem profiling")
	}

	if cpuProfile != "" {
		f, err := os.Create(cpuProfile)
		if err != nil {
			log.Fatal("could not create CPU profile: ", err)
		}
		defer f.Close()

		if err := pprof.StartCPUProfile(f); err != nil {
			log.Fatal("could not start CPU profile: ", err)
		}
		log.WithField("file", cpuProfile).Info("Starting cpu profiling")
	}

	bpf.CheckOrMountFS("")
	bpf.CheckOrMountDebugFS()
	bpf.CheckOrMountCgroup2()

	sensors.LogRegisteredSensorsAndProbes()

	bpf.ConfigureResourceLimits()
	observerDir := getObserverDir()
	option.Config.BpfDir = observerDir
	option.Config.MapDir = observerDir
	obs := observer.NewObserver(configFile)
	if err := obs.InitSensorManager(); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var cancelWg sync.WaitGroup

	/* Remove any stale programs, otherwise feature set change can cause
	 * old programs to linger resulting in undefined behavior. And because
	 * we recapture current running state from proc and/or have cache of
	 * events no state should be lost/missed.
	 */
	obs.RemovePrograms()
	os.Mkdir(defaults.DefaultRunDir, os.ModeDir)

	err := btf.InitCachedBTF(ctx, option.Config.HubbleLib, option.Config.BTF)
	if err != nil {
		return err
	}

	if runStandalone {
		if err := base.LoadDefault(ctx, observerDir, observerDir, option.Config.CiliumDir); err != nil {
			return err
		}
		return obs.StartStandalone(ctx)
	}

	if metricsServer != "" {
		go metrics.EnableMetrics(metricsServer)
	}

	watcher, err := getWatcher()
	if err != nil {
		return err
	}
	ciliumState, err := cilium.InitCiliumState(ctx, option.Config.EnableCilium)
	if err != nil {
		return err
	}

	if err := process.InitCache(ctx, watcher, option.Config.EnableCilium, processCacheSize); err != nil {
		return err
	}

	pm, err := tetragonGrpc.NewProcessManager(
		ctx,
		&cancelWg,
		ciliumState,
		observer.SensorManager)
	if err != nil {
		return err
	}
	if err = Serve(ctx, serverAddress, pm.Server); err != nil {
		return err
	}
	if exportFilename != "" {
		if err = startExporter(ctx, pm.Server); err != nil {
			return err
		}
	}

	go func() {
		<-sigs
		obs.PrintStats()
		obs.RemovePrograms()
		stopProfile()
		cancel()
		cancelWg.Wait()
		os.Exit(1)
	}()

	log.WithField("enabled", exportFilename != "").WithField("fileName", exportFilename).Info("Exporter configuration")
	obs.AddListener(pm)
	saveInitInfo()
	if option.Config.EnableK8s {
		go crd.WatchTracePolicy(ctx, observer.SensorManager)
	}

	var startSensors []*sensors.Sensor
	if len(configFile) > 0 {
		cnf, err := readConfig(configFile)
		if err != nil {
			return err
		}

		startSensors, err = sensors.GetSensorsFromParserPolicy(&cnf.Spec)
		if err != nil {
			return err
		}
	}

	if err := base.LoadDefault(ctx, observerDir, observerDir, option.Config.CiliumDir); err != nil {
		return err
	}

	return obs.Start(ctx, startSensors)
}

// getObserverDir returns the path to the observer directory based on the BPF
// map root. This function relies on the map root to be set properly via
// github.com/cilium/tetragon/pkg/bpf.CheckOrMountFS().
func getObserverDir() string {
	return bpf.MapPrefixPath()
}

func startExporter(ctx context.Context, server *server.Server) error {
	allowList, denyList, err := getExportFilters()
	if err != nil {
		return err
	}
	writer := &lumberjack.Logger{
		Filename:   exportFilename,
		MaxSize:    exportFileMaxSizeMB,
		MaxBackups: exportFileMaxBackups,
		Compress:   exportFileCompress,
	}
	if exportFileRotationInterval != 0 {
		log.WithField("duration", exportFileRotationInterval).Info("Periodically rotating JSON export files")
		go func() {
			ticker := time.NewTicker(exportFileRotationInterval)
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if rotationErr := writer.Rotate(); rotationErr != nil {
						log.WithError(rotationErr).
							WithField("filename", exportFilename).
							Warn("Failed to rotate JSON export file")
					}
				}
			}
		}()
	}
	encoder := json.NewEncoder(writer)
	var rateLimiter *ratelimit.RateLimiter
	if exportRateLimit >= 0 {
		rateLimiter = ratelimit.NewRateLimiter(ctx, 1*time.Minute, exportRateLimit, encoder)
	}
	var aggregationOptions *tetragon.AggregationOptions
	if enableExportAggregation {
		aggregationOptions = &tetragon.AggregationOptions{
			WindowSize:        durationpb.New(exportAggregationWindowSize),
			ChannelBufferSize: exportAggregationBufferSize,
		}
	}
	req := tetragon.GetEventsRequest{AllowList: allowList, DenyList: denyList, AggregationOptions: aggregationOptions}
	log.WithFields(logrus.Fields{"logger": writer, "request": &req}).Info("Starting JSON exporter")
	exporter := exporter.NewExporter(ctx, &req, server, encoder, writer, rateLimiter)
	exporter.Start()
	return nil
}

func Serve(ctx context.Context, address string, server *server.Server) error {
	grpcServer := grpc.NewServer()
	tetragon.RegisterFineGuidanceSensorsServer(grpcServer, server)
	go func(address string) {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			log.WithError(err).WithField("address", address).Fatal("Failed to start gRPC server")
		}
		log.WithField("address", address).Info("Starting gRPC server")
		if err = grpcServer.Serve(listener); err != nil {
			log.WithError(err).Error("Failed to close gRPC server")
		}
	}(address)
	go func() {
		<-ctx.Done()
		grpcServer.Stop()
	}()
	return nil
}

func getWatcher() (watcher.K8sResourceWatcher, error) {
	if option.Config.EnableK8s {
		log.Info("Enabling Kubernetes API")
		config, err := rest.InClusterConfig()
		if err != nil {
			return nil, err
		}
		k8sClient := kubernetes.NewForConfigOrDie(config)
		return watcher.NewK8sWatcher(k8sClient, 60*time.Second), nil

	}
	log.Info("Disabling Kubernetes API")
	return watcher.NewFakeK8sWatcher(nil), nil
}

func execute() error {
	rootCmd := &cobra.Command{
		Use:   "tetragon SOURCE_DIR BUCKET",
		Short: "Tetragon",
		Run: func(cmd *cobra.Command, args []string) {
			if err := gops.Listen(gops.Options{}); err != nil {
				log.WithError(err).Fatal("Failed to start gops")
			}
			if err := tetragonExecute(); err != nil {
				log.WithError(err).Fatal("Failed to start tetragon")
			}
		},
	}

	cobra.OnInitialize(func() {
		viper.SetEnvPrefix("tetragon")
		viper.SetConfigName("config")
		viper.SetConfigType("yaml")
		viper.AddConfigPath(".") // look for a config file in cwd first, useful during development
		if err := viper.ReadInConfig(); err == nil {
			log.Info("Loaded config from file")
		}
		if viper.IsSet(keyConfigDir) {
			configDir := viper.GetString(keyConfigDir)
			cm, err := option.ReadDirConfig(configDir)
			if err != nil {
				log.WithField(keyConfigDir, configDir).WithError(err).Fatal("Failed to read config from directory")
			}
			if err := viper.MergeConfigMap(cm); err != nil {
				log.WithField(keyConfigDir, configDir).WithError(err).Fatal("Failed to merge config from directory")
			}
			log.WithField(keyConfigDir, configDir).Info("Loaded config from directory")
		}
		replacer := strings.NewReplacer("-", "_")
		viper.SetEnvKeyReplacer(replacer)
		viper.AutomaticEnv()
	})

	flags := rootCmd.PersistentFlags()

	flags.String(keyConfigDir, "", "Configuration directory that contains a file for each option")
	flags.BoolP(keyDebug, "d", false, "Enable debug messages. Equivalent to '--log-level=debug'")
	flags.String(keyHubbleLib, defaults.DefaultTetragonLib, "Location of Tetragon libs (btf and bpf files)")
	flags.String(keyBTF, "", "Location of btf")

	flags.String(keyProcFS, "/proc/", "Location of procfs to consume existing PIDs")
	flags.String(keyKernelVersion, "", "Kernel version")
	flags.Int(keyVerbosity, 0, "set verbosity level")
	flags.Int(keyProcessCacheSize, 65536, "Size of the process cache")
	flags.Bool(keyForceSmallProgs, false, "Force loading small programs, even in kernels with >= 5.3 versions")
	flags.String(keyExportFilename, "", "Filename for JSON export. Disabled by default")
	flags.Int(keyExportFileMaxSizeMB, 10, "Size in MB for rotating JSON export files")
	flags.Duration(keyExportFileRotationInterval, 0, "Interval at which to rotate JSON export files in addition to rotating them by size")
	flags.Int(keyExportFileMaxBackups, 5, "Number of rotated JSON export files to retain")
	flags.Bool(keyExportFileCompress, false, "Compress rotated JSON export files")
	flags.Int(keyExportRateLimit, -1, "Rate limit (per minute) for event export. Set to -1 to disable")
	flags.String(keyLogLevel, "info", "Set log level")
	flags.String(keyLogFormat, "text", "Set log format")
	flags.Bool(keyEnableK8sAPI, false, "Access Kubernetes API to associate Tetragon events with Kubernetes pods")
	flags.Bool(keyEnableCiliumAPI, false, "Access Cilium API to associate Tetragon events with Cilium endpoints and DNS cache")
	flags.Bool(keyEnableProcessAncestors, true, "Include ancestors in process exec events")
	flags.String(keyMetricsServer, "", "Metrics server address (e.g. ':2112'). Set it to an empty string to disable.")
	flags.String(keyServerAddress, "localhost:54321", "gRPC server address")
	flags.String(keyCiliumBPF, "", "Cilium BPF directory")
	flags.Bool(keyEnableProcessCred, false, "Enable process_cred events")
	flags.Bool(keyEnableProcessNs, false, "Enable namespace information in process_exec and process_kprobe events")

	// Config files
	flags.String(keyConfigFile, "", "Configuration file to load from")

	// Options for debugging/development, not visible to users
	flags.Bool(keyRunStandalone, false, "Just start the observer and dump events to stdout")
	flags.MarkHidden(keyRunStandalone)

	flags.Bool(keyIgnoreMissingProgs, false, "Ignore missing BPF programs")
	flags.MarkHidden(keyIgnoreMissingProgs)

	flags.String(keyCpuProfile, "", "Store CPU profile into provided file")
	flags.MarkHidden(keyCpuProfile)

	flags.String(keyMemProfile, "", "Store MEM profile into provided file")
	flags.MarkHidden(keyMemProfile)

	// JSON export aggregation options.
	flags.Bool(keyEnableExportAggregation, false, "Enable JSON export aggregation")
	flags.Duration(keyExportAggregationWindowSize, 15*time.Second, "JSON export aggregation time window")
	flags.Uint64(keyExportAggregationBufferSize, 10000, "Aggregator channel buffer size")

	// JSON export filter options
	flags.String(keyExportAllowlist, "", "JSON export allowlist")
	flags.String(keyExportDenylist, "", "JSON export denylist")

	// Network namespace options
	flags.String(keyNetnsDir, "/var/run/docker/netns/", "Network namespace dir")

	viper.BindPFlags(flags)
	return rootCmd.Execute()
}
