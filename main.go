package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// ---------- types ----------

type SpeedTest struct {
	Iteration       int       `json:"iteration"`
	StartTime       time.Time `json:"startTime"`
	EndTime         time.Time `json:"endTime"`
	DurationSeconds float64   `json:"durationSeconds"`
	FileSizeBytes   int64     `json:"fileSizeBytes"`
	AvgSpeedMBps    float64   `json:"averageDownloadSpeedMBps"`
	AvgSpeedMbps    float64   `json:"averageDownloadSpeedMbps"`
	Success         bool      `json:"success"`
	Interrupted     bool      `json:"interrupted,omitempty"`
	Error           string    `json:"error,omitempty"`
}

type Summary struct {
	StartedAt         time.Time   `json:"startedAt"`
	FinishedAt        time.Time   `json:"finishedAt"`
	PlannedDuration   string      `json:"plannedDuration"`
	ActualDuration    string      `json:"actualDuration"`
	Source            string      `json:"source"`
	TotalRuns         int         `json:"totalRuns"`
	Successful        int         `json:"successful"`
	Failed            int         `json:"failed"`
	MinMBps           float64     `json:"minMBps"`
	MaxMBps           float64     `json:"maxMBps"`
	AvgMBps           float64     `json:"avgMBps"`
	SlowThresholdMBps float64     `json:"slowThresholdMBps"`
	SlowRuns          int         `json:"slowRuns"`
	SlowRunsPercent   float64     `json:"slowRunsPercent"`
	Tests             []SpeedTest `json:"tests"`
}

type Config struct {
	Source            string  `yaml:"source"`
	Duration          string  `yaml:"duration"`
	Port              int     `yaml:"port"`
	SSHKey            string  `yaml:"ssh_key"`
	LogsDir           string  `yaml:"logs_dir"`
	Interval          string  `yaml:"interval"`
	ScpArgs           string  `yaml:"scp_args"`
	SlowThresholdMBps float64 `yaml:"slow_threshold_mbps"`
}

func defaultConfig() Config {
	return Config{
		Duration:          "5m",
		Port:              22,
		LogsDir:           "logs",
		Interval:          "0s",
		SlowThresholdMBps: 5,
	}
}

// ---------- main ----------

func main() {
	cfg := defaultConfig()

	var (
		configPath = flag.String("config", "", "path to YAML config file (e.g. config.yaml)")
		fSource    = flag.String("source", "", "scp source: user@ip:/path/to/file")
		fDuration  = flag.String("duration", "", "how long to run: 30s, 5m, 1h, 24h, 1h30m")
		fPort      = flag.Int("p", 0, "ssh port (0 = use config / default 22)")
		fKey       = flag.String("i", "", "ssh identity file")
		fLogs      = flag.String("logs", "", "logs output directory")
		fInterval  = flag.String("interval", "", "pause between iterations, e.g. 2s")
		fScpArgs   = flag.String("scp-args", "", "extra args to pass to scp")
		fSlow      = flag.Float64("slow-threshold", -1, "slow threshold in MB/s (-1 = use config / default 5)")
	)
	flag.Parse()

	// Resolve which config file to use:
	//   1. -config flag if given (must exist, error if not)
	//   2. ./config.yaml in current dir if it exists (silent fallback)
	//   3. otherwise, no config file (defaults + flags only)
	resolvedConfig := *configPath
	configWasExplicit := *configPath != ""
	if !configWasExplicit {
		if _, err := os.Stat("config.yaml"); err == nil {
			resolvedConfig = "config.yaml"
		}
	}
	if resolvedConfig != "" {
		loaded, err := loadConfig(resolvedConfig)
		if err != nil {
			if configWasExplicit {
				fmt.Fprintf(os.Stderr, "ERROR: cannot load config %q: %v\n", resolvedConfig, err)
				os.Exit(2)
			}
			// auto-discovered config exists but is broken — warn but keep going
			fmt.Fprintf(os.Stderr, "WARN: found ./config.yaml but cannot load it: %v\n", err)
			resolvedConfig = ""
		} else {
			cfg = mergeConfig(cfg, loaded)
		}
	}

	// flags override config
	if *fSource != "" {
		cfg.Source = *fSource
	}
	if *fDuration != "" {
		cfg.Duration = *fDuration
	}
	if *fPort != 0 {
		cfg.Port = *fPort
	}
	if *fKey != "" {
		cfg.SSHKey = *fKey
	}
	if *fLogs != "" {
		cfg.LogsDir = *fLogs
	}
	if *fInterval != "" {
		cfg.Interval = *fInterval
	}
	if *fScpArgs != "" {
		cfg.ScpArgs = *fScpArgs
	}
	if *fSlow >= 0 {
		cfg.SlowThresholdMBps = *fSlow
	}

	if cfg.Source == "" {
		fmt.Fprintln(os.Stderr, "ERROR: source is required (set in config.yaml or pass -source user@ip:/path)")
		flag.Usage()
		os.Exit(2)
	}
	plannedDur, err := time.ParseDuration(cfg.Duration)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: invalid duration %q: %v\n", cfg.Duration, err)
		os.Exit(2)
	}
	intervalDur := time.Duration(0)
	if cfg.Interval != "" && cfg.Interval != "0s" && cfg.Interval != "0" {
		intervalDur, err = time.ParseDuration(cfg.Interval)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: invalid interval %q: %v\n", cfg.Interval, err)
			os.Exit(2)
		}
	}

	if err := os.MkdirAll(cfg.LogsDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: cannot create logs dir: %v\n", err)
		os.Exit(1)
	}

	runStart := time.Now()
	stamp := runStart.Format("20060102-150405")
	logPath := filepath.Join(cfg.LogsDir, fmt.Sprintf("speedtest-%s.log", stamp))
	jsonPath := filepath.Join(cfg.LogsDir, fmt.Sprintf("speedtest-%s.json", stamp))

	logFile, err := os.Create(logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: cannot create log file: %v\n", err)
		os.Exit(1)
	}
	defer logFile.Close()

	logger := log.New(io.MultiWriter(os.Stdout, logFile), "", log.LstdFlags)

	logger.Printf("=== speedtest started ===")
	if resolvedConfig != "" {
		logger.Printf("config:         %s", resolvedConfig)
	} else {
		logger.Printf("config:         (none — using defaults + flags)")
	}
	logger.Printf("source:         %s", cfg.Source)
	logger.Printf("port:           %d", cfg.Port)
	logger.Printf("duration:       %s", plannedDur)
	logger.Printf("interval:       %s", intervalDur)
	logger.Printf("slow threshold: %.2f MB/s", cfg.SlowThresholdMBps)
	logger.Printf("logs dir:       %s", cfg.LogsDir)
	logger.Printf("log file:       %s", logPath)
	logger.Printf("json file:      %s", jsonPath)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	deadline := runStart.Add(plannedDur)
	results := make([]SpeedTest, 0, 64)
	var mu sync.Mutex

	flushJSON := func(end time.Time) {
		mu.Lock()
		defer mu.Unlock()
		summary := buildSummary(results, runStart, end, cfg.Duration, cfg.Source, cfg.SlowThresholdMBps)
		if err := writeJSON(jsonPath, summary); err != nil {
			logger.Printf("WARN: failed to write json: %v", err)
		}
	}

	var stop atomic.Bool
	go func() {
		s := <-sigCh
		logger.Printf("got signal %v — stopping after current iteration", s)
		stop.Store(true)
	}()

	tmpDir, err := os.MkdirTemp("", "speedtest-")
	if err != nil {
		logger.Fatalf("cannot create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	iter := 0
	for {
		if stop.Load() {
			break
		}
		if time.Now().After(deadline) {
			logger.Printf("planned duration reached")
			break
		}
		iter++
		test := runOne(iter, cfg, tmpDir, logger, &stop)

		mu.Lock()
		results = append(results, test)
		mu.Unlock()

		flushJSON(time.Now())

		if intervalDur > 0 && !stop.Load() && time.Now().Before(deadline) {
			time.Sleep(intervalDur)
		}
	}

	end := time.Now()
	flushJSON(end)
	logger.Printf("=== speedtest finished ===")
	logger.Printf("total runs: %d   wrote: %s", len(results), jsonPath)
}

// ---------- config ----------

func loadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		return c, err
	}
	return c, nil
}

func mergeConfig(base, over Config) Config {
	if over.Source != "" {
		base.Source = over.Source
	}
	if over.Duration != "" {
		base.Duration = over.Duration
	}
	if over.Port != 0 {
		base.Port = over.Port
	}
	if over.SSHKey != "" {
		base.SSHKey = over.SSHKey
	}
	if over.LogsDir != "" {
		base.LogsDir = over.LogsDir
	}
	if over.Interval != "" {
		base.Interval = over.Interval
	}
	if over.ScpArgs != "" {
		base.ScpArgs = over.ScpArgs
	}
	if over.SlowThresholdMBps != 0 {
		base.SlowThresholdMBps = over.SlowThresholdMBps
	}
	return base
}

// ---------- one iteration ----------

func runOne(iter int, cfg Config, tmpDir string, logger *log.Logger, stop *atomic.Bool) SpeedTest {
	t := SpeedTest{Iteration: iter, StartTime: time.Now()}

	base := filepath.Base(cfg.Source)
	if base == "" || base == "." || base == "/" {
		base = fmt.Sprintf("download-%d.bin", iter)
	}
	dest := filepath.Join(tmpDir, fmt.Sprintf("%d-%s", iter, base))

	args := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new"}
	if cfg.SSHKey != "" {
		args = append(args, "-i", cfg.SSHKey)
	}
	if cfg.Port != 0 && cfg.Port != 22 {
		args = append(args, "-P", strconv.Itoa(cfg.Port))
	}
	if cfg.ScpArgs != "" {
		args = append(args, strings.Fields(cfg.ScpArgs)...)
	}
	args = append(args, cfg.Source, dest)

	logger.Printf("[#%d] start  scp %s", iter, cfg.Source)

	cmd := exec.Command("scp", args...)
	stderrPipe, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		t.EndTime = time.Now()
		t.DurationSeconds = t.EndTime.Sub(t.StartTime).Seconds()
		t.Error = fmt.Sprintf("scp start failed: %v", err)
		logger.Printf("[#%d] FAIL   %.2fs  %s", iter, t.DurationSeconds, t.Error)
		return t
	}

	stderrBuf := &strings.Builder{}
	go func() {
		sc := bufio.NewScanner(stderrPipe)
		for sc.Scan() {
			stderrBuf.WriteString(sc.Text())
			stderrBuf.WriteString("\n")
		}
	}()

	err := cmd.Wait()
	t.EndTime = time.Now()
	t.DurationSeconds = t.EndTime.Sub(t.StartTime).Seconds()

	if err != nil {
		if stop.Load() {
			t.Interrupted = true
			t.Error = fmt.Sprintf("manually stopped: %v", err)
			logger.Printf("[#%d] STOPPED  manually interrupted after %.2fs", iter, t.DurationSeconds)
		} else {
			t.Error = fmt.Sprintf("scp failed: %v — stderr: %s", err, strings.TrimSpace(stderrBuf.String()))
			logger.Printf("[#%d] FAIL   %.2fs  %s", iter, t.DurationSeconds, t.Error)
		}
		_ = os.Remove(dest)
		return t
	}

	info, statErr := os.Stat(dest)
	if statErr != nil {
		t.Error = fmt.Sprintf("stat downloaded file failed: %v", statErr)
		logger.Printf("[#%d] FAIL   %s", iter, t.Error)
		return t
	}
	t.FileSizeBytes = info.Size()
	if t.DurationSeconds > 0 {
		t.AvgSpeedMBps = float64(t.FileSizeBytes) / (1024 * 1024) / t.DurationSeconds
		t.AvgSpeedMbps = (float64(t.FileSizeBytes) * 8) / (1000 * 1000) / t.DurationSeconds
	}
	t.Success = true

	if rmErr := os.Remove(dest); rmErr != nil {
		logger.Printf("[#%d] warn   cleanup failed: %v", iter, rmErr)
	}

	logger.Printf("[#%d] ok     %s in %.2fs  =>  %.2f MB/s  (%.2f Mbps)",
		iter, humanSize(t.FileSizeBytes), t.DurationSeconds, t.AvgSpeedMBps, t.AvgSpeedMbps)
	return t
}

// ---------- summary ----------

func buildSummary(tests []SpeedTest, start, end time.Time, planned, source string, slowThreshold float64) Summary {
	s := Summary{
		StartedAt:         start,
		FinishedAt:        end,
		PlannedDuration:   planned,
		ActualDuration:    end.Sub(start).Round(time.Second).String(),
		Source:            source,
		TotalRuns:         len(tests),
		SlowThresholdMBps: slowThreshold,
		Tests:             tests,
	}
	var sum float64
	first := true
	for _, t := range tests {
		if !t.Success {
			s.Failed++
			continue
		}
		s.Successful++
		sum += t.AvgSpeedMBps
		if first {
			s.MinMBps = t.AvgSpeedMBps
			s.MaxMBps = t.AvgSpeedMBps
			first = false
		} else {
			if t.AvgSpeedMBps < s.MinMBps {
				s.MinMBps = t.AvgSpeedMBps
			}
			if t.AvgSpeedMBps > s.MaxMBps {
				s.MaxMBps = t.AvgSpeedMBps
			}
		}

		if t.AvgSpeedMBps < slowThreshold {
			s.SlowRuns++
		}
	}
	if s.Successful > 0 {
		s.AvgMBps = sum / float64(s.Successful)
		s.SlowRunsPercent = float64(s.SlowRuns) / float64(s.Successful) * 100
	}
	return s
}

// ---------- helpers ----------

func writeJSON(path string, v any) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func humanSize(b int64) string {
	const u = 1024.0
	if b < int64(u) {
		return fmt.Sprintf("%d B", b)
	}
	x := float64(b)
	units := []string{"KB", "MB", "GB", "TB"}
	i := -1
	for x >= u && i < len(units)-1 {
		x /= u
		i++
	}
	return fmt.Sprintf("%.2f %s", x, units[i])
}
