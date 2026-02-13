# Weather Service

Go service that wraps the [NWS API](https://www.weather.gov/documentation/services-web-api) to return a simple weather forecast for a given lat/lon. Standard library only, no external dependencies.

## Run Instructions

In your terminal, navigate to the project directory and run:

```bash
cd golang-weather-api-jh
go run .
```

Starts the server on `:8080` and launches an interactive menu where you can look up weather by lat/lon or city name. The API is also available via curl:

```bash
curl localhost:8080/health
curl "localhost:8080/weather?lat=42.8294&lon=-98.3289"
```

Response:
```json
{
  "short_forecast": "Mostly Sunny",
  "temp_characterization": "Cold",
  "metadata": { "city": "Monowi", "state": "NE" }
}
```

## What I'd change in production

- Swap `log.Printf` for `slog` or `zerolog` (structured JSON logs)
- Move config to env vars instead of constants
- Add rate limiting — NWS enforces soft limits (~5 req/s)
- Use Redis instead of in-memory cache (survives restarts, bounded size)
- Add a circuit breaker (`sony/gobreaker`) around the NWS client
- Unit tests + `httptest` integration tests
