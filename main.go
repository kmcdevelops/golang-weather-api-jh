package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Hardcoded for simplicity — would pull from env vars in a real service.
const (
	serverAddr       = ":8080"
	nwsBaseURL       = "https://api.weather.gov"
	nwsTimeout       = 5 * time.Second
	cacheTTL         = 1 * time.Minute
	shutdownDeadline = 10 * time.Second
)

// --- Domain types ---
// City/state aren't in the spec, but as a user I didn't want to look up
// coordinates and wonder what city they map to, so I hooked that up real quick.

type WeatherResponse struct {
	ShortForecast        string   `json:"short_forecast"`
	TempCharacterization string   `json:"temp_characterization"`
	Metadata             Metadata `json:"metadata"`
}

type Metadata struct {
	City  string `json:"city"`
	State string `json:"state"`
}

// --- NWS API responses ---

type nwsPointsResponse struct {
	Properties struct {
		Forecast         string `json:"forecast"`
		RelativeLocation struct {
			Properties struct {
				City  string `json:"city"`
				State string `json:"state"`
			} `json:"properties"`
		} `json:"relativeLocation"`
	} `json:"properties"`
}

type nwsForecastResponse struct {
	Properties struct {
		Periods []struct {
			Name          string `json:"name"`
			Temperature   int    `json:"temperature"`
			ShortForecast string `json:"shortForecast"`
		} `json:"periods"`
	} `json:"properties"`
}

// --- In-memory cache ---
//
// Not required by the spec — added this for fun. The NWS API is slow
// (~1-2s per call) so caching felt like the obvious thing to do. Went
// with map + RWMutex over sync.Map because we need TTL expiry.
// In production I'd swap this for Redis.

type cacheEntry struct {
	data      WeatherResponse
	expiresAt time.Time
}

type weatherCache struct {
	mu      sync.RWMutex
	entries map[string]cacheEntry
}

func newWeatherCache() *weatherCache {
	return &weatherCache{entries: make(map[string]cacheEntry)}
}

func (c *weatherCache) Get(key string) (WeatherResponse, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, ok := c.entries[key]
	if !ok || time.Now().After(entry.expiresAt) {
		return WeatherResponse{}, false
	}
	return entry.data, true
}

func (c *weatherCache) Set(key string, data WeatherResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[key] = cacheEntry{
		data:      data,
		expiresAt: time.Now().Add(cacheTTL),
	}
}

// --- NWS client ---
//
// NWS is a two-step process: hit /points with coordinates to get a
// forecast URL, then fetch the forecast. Context propagates cancellations.
//
// Kept these as flat functions for simplicity. In a larger codebase I'd
// wrap them in an NWSClient struct so it's easier to mock for testing.

func fetchWeather(ctx context.Context, lat, lon float64) (WeatherResponse, error) {
	pointsURL := fmt.Sprintf("%s/points/%.4f,%.4f", nwsBaseURL, lat, lon)

	// Step 1 — resolve coordinates to a forecast URL and location info.
	var points nwsPointsResponse
	if err := nwsGet(ctx, pointsURL, &points); err != nil {
		return WeatherResponse{}, fmt.Errorf("points lookup: %w", err)
	}

	forecastURL := points.Properties.Forecast
	if forecastURL == "" {
		return WeatherResponse{}, fmt.Errorf("no forecast URL for (%g, %g)", lat, lon)
	}

	// Step 2 — fetch the forecast.
	var forecast nwsForecastResponse
	if err := nwsGet(ctx, forecastURL, &forecast); err != nil {
		return WeatherResponse{}, fmt.Errorf("forecast fetch: %w", err)
	}

	if len(forecast.Properties.Periods) == 0 {
		return WeatherResponse{}, fmt.Errorf("NWS returned empty forecast")
	}

	period := forecast.Properties.Periods[0]

	return WeatherResponse{
		ShortForecast:        period.ShortForecast,
		TempCharacterization: characterizeTemp(period.Temperature),
		Metadata: Metadata{
			City:  points.Properties.RelativeLocation.Properties.City,
			State: points.Properties.RelativeLocation.Properties.State,
		},
	}, nil
}

// nwsGet makes a GET to the NWS API with a timeout.
func nwsGet(ctx context.Context, url string, dest interface{}) error {
	ctx, cancel := context.WithTimeout(ctx, nwsTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	// NWS requires a User-Agent or they'll 403 you.
	req.Header.Set("User-Agent", "(golang-weather-api-jh, contact@example.com)")
	req.Header.Set("Accept", "application/geo+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// If NWS is having a bad day, surface that clearly to our caller.
	if resp.StatusCode >= 500 {
		return fmt.Errorf("NWS returned %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	return json.NewDecoder(resp.Body).Decode(dest)
}

func characterizeTemp(tempF int) string {
	switch {
	case tempF < 55:
		return "Cold"
	case tempF > 85:
		return "Hot"
	default:
		return "Moderate"
	}
}

// --- Handlers ---

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleWeather(cache *weatherCache) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		latStr := r.URL.Query().Get("lat")
		lonStr := r.URL.Query().Get("lon")

		if latStr == "" || lonStr == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "missing required query parameters: lat and lon",
			})
			return
		}

		lat, err := strconv.ParseFloat(latStr, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "lat must be a valid number",
			})
			return
		}

		lon, err := strconv.ParseFloat(lonStr, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "lon must be a valid number",
			})
			return
		}

		// TODO: could normalize precision for smarter cache hits,
		// but just sanitizing input for now so the key is consistent.
		cacheKey := fmt.Sprintf("%g,%g", lat, lon)

		if cached, ok := cache.Get(cacheKey); ok {
			log.Printf("cache hit for %s", cacheKey)
			writeJSON(w, http.StatusOK, cached)
			return
		}

		log.Printf("cache miss for %s — calling NWS", cacheKey)

		result, err := fetchWeather(r.Context(), lat, lon)
		if err != nil {
			log.Printf("NWS error: %v", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error": "Weather data is temporarily unavailable. Please try again shortly.",
			})
			return
		}

		cache.Set(cacheKey, result)
		writeJSON(w, http.StatusOK, result)
	}
}

// --- Logging middleware ---
//
// Not in the spec — just added it for flair. Logs method, path, status,
// and duration on every request. Would use slog/zerolog in production.

type statusRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.statusCode = code
	sr.ResponseWriter.WriteHeader(code)
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.statusCode, time.Since(start).Round(time.Millisecond))
	})
}

// --- Helpers ---

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// --- Interactive CLI ---
//
// Added a quick interactive menu so you don't have to curl manually.
// Geocodes city/state via OpenStreetMap Nominatim (free, no API key).

func runCLI(shutdown chan<- os.Signal) {
	scanner := bufio.NewScanner(os.Stdin)

	fmt.Println()
	fmt.Println("=================================")
	fmt.Println("  Weather Service - Go API Caller")
	fmt.Println("=================================")
	fmt.Println()

	for {
		fmt.Println("1) Enter lat/lon")
		fmt.Println("2) Enter city, state")
		fmt.Println("3) Exit")
		fmt.Print("\n> ")

		if !scanner.Scan() {
			break
		}
		choice := strings.TrimSpace(scanner.Text())

		switch choice {
		case "1":
			fmt.Print("Latitude: ")
			scanner.Scan()
			lat := strings.TrimSpace(scanner.Text())

			fmt.Print("Longitude: ")
			scanner.Scan()
			lon := strings.TrimSpace(scanner.Text())

			callWeatherAPI(lat, lon)

		case "2":
			fmt.Print("City, State (e.g. Austin, TX): ")
			scanner.Scan()
			input := strings.TrimSpace(scanner.Text())

			lat, lon, err := geocode(input)
			if err != nil {
				fmt.Printf("\n  ❌ Couldn't geocode \"%s\": %v\n\n", input, err)
				continue
			}
			fmt.Printf("  → Resolved to %.4f, %.4f\n", lat, lon)
			callWeatherAPI(fmt.Sprintf("%f", lat), fmt.Sprintf("%f", lon))

		case "3":
			fmt.Println("Bye!")
			shutdown <- syscall.SIGTERM
			return

		default:
			fmt.Println("  Pick 1, 2, or 3.")
		}
	}
}

// tryWeatherAPI makes a single call and returns the status code + parsed body.
func tryWeatherAPI(lat, lon string) (int, *WeatherResponse, error) {
	resp, err := http.Get(fmt.Sprintf("http://localhost%s/weather?lat=%s&lon=%s", serverAddr, lat, lon))
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return resp.StatusCode, nil, nil
	}

	var w WeatherResponse
	json.NewDecoder(resp.Body).Decode(&w)
	return 200, &w, nil
}

// callWeatherAPI tries the coordinates as is, and if NWS rejects them and
// the longitude is positive, retries with a negative value. All US longitudes
// are negative (western hemisphere) but people rarely type the minus sign,
// so this just saves a round-trip of confusion.
func callWeatherAPI(lat, lon string) {
	// Try the coordinates as given.
	fmt.Printf("  → Trying (%s, %s)...\n", lat, lon)
	status, w, err := tryWeatherAPI(lat, lon)
	if err != nil {
		fmt.Printf("\n  ❌ Server error: %v\n\n", err)
		return
	}

	// If it failed and longitude is positive, the user probably forgot
	// the negative sign (US is western hemisphere). Try flipping it.
	if status != 200 {
		if lonVal, parseErr := strconv.ParseFloat(lon, 64); parseErr == nil && lonVal > 0 {
			flippedLon := fmt.Sprintf("%f", -lonVal)
			fmt.Printf("  → NWS returned %d — retrying with negative longitude (%s, %s)...\n", status, lat, flippedLon)
			status, w, err = tryWeatherAPI(lat, flippedLon)
			if err != nil {
				fmt.Printf("\n  ❌ Server error: %v\n\n", err)
				return
			}
		}
	}

	if status != 200 || w == nil {
		fmt.Printf("\n  ❌ Weather data unavailable (HTTP %d). Check your coordinates.\n\n", status)
		return
	}

	fmt.Println()
	fmt.Println("  ┌─────────────────────────────────┐")
	fmt.Printf("  │  📍 %s, %s\n", w.Metadata.City, w.Metadata.State)
	fmt.Printf("  │  🌤  %s\n", w.ShortForecast)
	fmt.Printf("  │  🌡  %s\n", w.TempCharacterization)
	fmt.Println("  └─────────────────────────────────┘")
	fmt.Println()
}

func geocode(query string) (float64, float64, error) {
	u := fmt.Sprintf("https://nominatim.openstreetmap.org/search?q=%s&format=json&limit=1&countrycodes=us",
		url.QueryEscape(query))

	client := &http.Client{Timeout: 5 * time.Second}
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", "golang-weather-api-jh")

	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, fmt.Errorf("geocoding failed: %w", err)
	}
	defer resp.Body.Close()

	var results []struct {
		Lat string `json:"lat"`
		Lon string `json:"lon"`
	}
	json.NewDecoder(resp.Body).Decode(&results)

	if len(results) == 0 {
		return 0, 0, fmt.Errorf("no results found")
	}

	var lat, lon float64
	fmt.Sscanf(results[0].Lat, "%f", &lat)
	fmt.Sscanf(results[0].Lon, "%f", &lon)

	return lat, lon, nil
}

// --- Main ---

func main() {
	cache := newWeatherCache()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/weather", handleWeather(cache))

	handler := loggingMiddleware(mux)

	srv := &http.Server{
		Addr:         serverAddr,
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Run the server in a goroutine so main can run the interactive CLI.
	go func() {
		log.Printf("weather service listening on %s", serverAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	// Give the server a moment to start before showing the menu.
	time.Sleep(100 * time.Millisecond)

	// Signal channel — either the CLI sends SIGTERM on exit, or the user
	// hits Ctrl+C. Either way we shut down cleanly.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)

	// Run the interactive menu on the main goroutine.
	go runCLI(quit)

	sig := <-quit
	log.Printf("received %s, shutting down", sig)

	ctx, cancel := context.WithTimeout(context.Background(), shutdownDeadline)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("forced shutdown: %v", err)
	}
	log.Println("server stopped")
}

// TODO: things I'd add with more time:
// - structured logging (slog/zerolog)
// - rate limiting (NWS has soft limits)
// - unit tests (table-driven for characterizeTemp, httptest for handlers)
// - circuit breaker around NWS client
// - bounded cache size (LRU or sweep goroutine)
