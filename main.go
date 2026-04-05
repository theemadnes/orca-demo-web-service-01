package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	orcapb "github.com/cncf/xds/go/xds/data/orca/v3"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/mem"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Response represents the simple hello world payload.
type Response struct {
	Message   string  `json:"message"`
	Zone      *string `json:"zone"`
	Hostname  string  `json:"hostname"`
	Timestamp string  `json:"timestamp"`
}

var instanceZone *string

func fetchZone() {
	url := "http://metadata.google.internal/computeMetadata/v1/instance/zone"
	client := &http.Client{Timeout: 2 * time.Second}

	for i := 0; i < 3; i++ {
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			log.Printf("Failed to create metadata request: %v", err)
			continue
		}
		req.Header.Set("Metadata-Flavor", "Google")

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("Metadata request failed (attempt %d): %v", i+1, err)
			time.Sleep(1 * time.Second)
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			body, err := io.ReadAll(resp.Body)
			if err == nil {
				zonePath := string(body)
				parts := strings.Split(zonePath, "/")
				zone := parts[len(parts)-1]
				instanceZone = &zone
				log.Printf("Detected zone: %s", zone)
				return
			}
		}
		log.Printf("Metadata request returned status %d (attempt %d)", resp.StatusCode, i+1)
		time.Sleep(1 * time.Second)
	}
	log.Println("GCE metadata zone unavailable after 3 retries")
}

var (
	// currentMetrics stores the moving average calculated every interval
	currentMetrics orcapb.OrcaLoadReport
	metricsMutex   sync.RWMutex

	// counters track the raw request and error counts
	statsMutex        sync.Mutex
	totalRequestCount int64
	totalErrorCount   int64

	// state for the calculation window
	lastRequestCount int64
	lastErrorCount   int64
	lastUpdateTime   time.Time

	// Prometheus Metrics
	promCPUUtilization = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "orca_cpu_utilization",
		Help: "CPU utilization fraction (0.0-1.0)",
	})
	promMemUtilization = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "orca_mem_utilization",
		Help: "Memory utilization fraction (0.0-1.0)",
	})
	promAppUtilization = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "orca_application_utilization",
		Help: "Application utilization fraction (0.0-1.0)",
	})
	promRPS = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "orca_rps",
		Help: "Requests per second",
	})
	promEPS = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "orca_eps",
		Help: "Errors per second",
	})
	promTotalRequests = promauto.NewCounter(prometheus.CounterOpts{
		Name: "orca_total_requests_total",
		Help: "Total number of requests",
	})
	promTotalErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "orca_total_errors_total",
		Help: "Total number of errors (5xx)",
	})
)

// statusResponseWriter captures the status code written to the response.
type statusResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (w *statusResponseWriter) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

// orcaMetricsMiddleware wraps an http.HandlerFunc to track requests and errors.
func orcaMetricsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	// JSON marshaler that emits unpopulated fields if necessary,
	// but standard ORCA usually prefers snake_case.
	marshaler := protojson.MarshalOptions{
		UseProtoNames: true, // Uses snake_case field names from .proto
	}

	return func(w http.ResponseWriter, r *http.Request) {
		// 1. Fetch the latest calculated ORCA metrics and set the header.
		metricsMutex.RLock()
		report := &currentMetrics
		orcaBytes, err := marshaler.Marshal(report)
		metricsMutex.RUnlock()

		if err == nil {
			w.Header().Set("endpoint-load-metrics", fmt.Sprintf("JSON %s", string(orcaBytes)))
		}

		// 2. Wrap ResponseWriter to capture the status code
		sw := &statusResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		// 3. Serve the request
		next.ServeHTTP(sw, r)

		// 4. Increment counts after completion for consistency in the window
		statsMutex.Lock()
		totalRequestCount++
		promTotalRequests.Inc()
		if sw.statusCode >= 500 {
			totalErrorCount++
			promTotalErrors.Inc()
		}
		statsMutex.Unlock()
	}
}

// updateMetrics periodically updates system utilization and calculates RPS/EPS averages.
func updateMetrics(ctx context.Context) {
	interval := 5 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Initialize state
	statsMutex.Lock()
	lastUpdateTime = time.Now()
	lastRequestCount = totalRequestCount
	lastErrorCount = totalErrorCount
	statsMutex.Unlock()

	for {
		select {
		case <-ctx.Done():
			log.Println("Metrics update stopping...")
			return
		case <-ticker.C:
			// 1. Sample current counts and time immediately to define the window
			statsMutex.Lock()
			now := time.Now()
			snapTotalRequests := totalRequestCount
			snapTotalErrors := totalErrorCount

			elapsed := now.Sub(lastUpdateTime).Seconds()

			// Calculate delta since last window
			deltaRequests := snapTotalRequests - lastRequestCount
			deltaErrors := snapTotalErrors - lastErrorCount

			// Update state for next window
			lastRequestCount = snapTotalRequests
			lastErrorCount = snapTotalErrors
			lastUpdateTime = now
			statsMutex.Unlock()

			// 2. Calculate RPS and EPS (avoid division by zero)
			var rps, eps float64
			if elapsed > 0 {
				rps = float64(deltaRequests) / elapsed
				eps = float64(deltaErrors) / elapsed
			}

			// 3. Get System Metrics (blocks for 1s)
			cpuPercent, _ := cpu.Percent(time.Second, false)
			vmStat, _ := mem.VirtualMemory()

			// 4. Update the global report state
			metricsMutex.Lock()
			if len(cpuPercent) > 0 {
				val := cpuPercent[0] / 100.0
				currentMetrics.CpuUtilization = val
				promCPUUtilization.Set(val)
			}
			if vmStat != nil {
				val := vmStat.UsedPercent / 100.0
				currentMetrics.MemUtilization = val
				promMemUtilization.Set(val)
			}
			currentMetrics.ApplicationUtilization = 0.1
			promAppUtilization.Set(0.1)

			currentMetrics.RpsFractional = rps
			promRPS.Set(rps)

			currentMetrics.Eps = eps
			promEPS.Set(eps)

			currentMetrics.NamedMetrics = map[string]float64{
				"total_requests": float64(snapTotalRequests),
				"total_errors":   float64(snapTotalErrors),
			}
			metricsMutex.Unlock()
		}
	}
}

func helloHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	if r.URL.Query().Get("error") == "true" {
		http.Error(w, "Triggered Error", http.StatusInternalServerError)
		return
	}

	hostname, _ := os.Hostname()

	w.Header().Set("Content-Type", "application/json")
	resp := Response{
		Message:   "Hello, World!",
		Zone:      instanceZone,
		Hostname:  hostname,
		Timestamp: time.Now().Format(time.RFC3339),
	}
	json.NewEncoder(w).Encode(resp)
}

func main() {
	// Query GCE metadata for zone at startup
	fetchZone()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start metrics background update goroutine
	go updateMetrics(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/", orcaMetricsMiddleware(helloHandler))
	mux.Handle("/metrics", promhttp.Handler())

	port := ":8080"
	server := &http.Server{
		Addr:    port,
		Handler: mux,
	}

	// Server run context
	serverCtx, serverStopCtx := context.WithCancel(context.Background())

	// Listen for syscall signals for process to interrupt/quit
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		<-sig

		// Shutdown signal with grace period of 30 seconds
		shutdownCtx, _ := context.WithTimeout(serverCtx, 30*time.Second)

		go func() {
			<-shutdownCtx.Done()
			if shutdownCtx.Err() == context.DeadlineExceeded {
				log.Fatal("graceful shutdown timed out.. forcing exit.")
			}
		}()

		// Trigger graceful shutdown
		err := server.Shutdown(shutdownCtx)
		if err != nil {
			log.Fatal(err)
		}
		serverStopCtx()
	}()

	log.Printf("Server starting on %s...", port)
	err := server.ListenAndServe()
	if err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}

	// Wait for server context to be stopped
	<-serverCtx.Done()
}
