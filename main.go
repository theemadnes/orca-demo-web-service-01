package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/mem"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Response represents the simple hello world payload.
type Response struct {
	Message string `json:"message"`
}

// ORCALoadReport represents the load metrics for ORCA.
type ORCALoadReport struct {
	CPUUtilization         float64            `json:"cpu_utilization,omitempty"`
	MemUtilization         float64            `json:"mem_utilization,omitempty"`
	ApplicationUtilization float64            `json:"application_utilization,omitempty"`
	RPSFractional          float64            `json:"rps_fractional"` // Mandatory for Weighted Round Robin
	EPS                    float64            `json:"eps"`            // Mandatory for Weighted Round Robin
	NamedMetrics           map[string]float64 `json:"named_metrics,omitempty"`
}

var (
	// currentMetrics stores the moving average calculated every interval
	currentMetrics ORCALoadReport
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
	return func(w http.ResponseWriter, r *http.Request) {
		// 1. Fetch the latest calculated ORCA metrics and set the header.
		// These reflect the average from the previous 5-second window.
		metricsMutex.RLock()
		report := currentMetrics
		metricsMutex.RUnlock()

		if orcaBytes, err := json.Marshal(report); err == nil {
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
		// Only count 5xx (Server Errors) as EPS.
		// 4xx (Client Errors like 404) are usually excluded from ORCA error rates.
		if sw.statusCode >= 500 {
			totalErrorCount++
			promTotalErrors.Inc()
		}
		statsMutex.Unlock()
	}
}

// updateMetrics periodically updates system utilization and calculates RPS/EPS averages.
func updateMetrics() {
	interval := 5 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Initialize state
	statsMutex.Lock()
	lastUpdateTime = time.Now()
	lastRequestCount = totalRequestCount
	lastErrorCount = totalErrorCount
	statsMutex.Unlock()

	for range ticker.C {
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
			currentMetrics.CPUUtilization = val
			promCPUUtilization.Set(val)
		}
		if vmStat != nil {
			val := vmStat.UsedPercent / 100.0
			currentMetrics.MemUtilization = val
			promMemUtilization.Set(val)
		}
		currentMetrics.ApplicationUtilization = 0.1
		promAppUtilization.Set(0.1)

		currentMetrics.RPSFractional = rps
		promRPS.Set(rps)

		currentMetrics.EPS = eps
		promEPS.Set(eps)

		currentMetrics.NamedMetrics = map[string]float64{
			"total_requests": float64(snapTotalRequests),
			"total_errors":   float64(snapTotalErrors),
		}
		metricsMutex.Unlock()
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

	w.Header().Set("Content-Type", "application/json")
	resp := Response{Message: "Hello, World!"}
	json.NewEncoder(w).Encode(resp)
}

func main() {
	// Start metrics background update goroutine
	go updateMetrics()

	http.HandleFunc("/", orcaMetricsMiddleware(helloHandler))
	http.Handle("/metrics", promhttp.Handler())

	port := ":8080"
	log.Printf("Server starting on %s...", port)
	if err := http.ListenAndServe(port, nil); err != nil {
		log.Fatal(err)
	}
}
