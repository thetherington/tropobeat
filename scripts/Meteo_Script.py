import json
import time
from datetime import datetime, timezone
import requests

# ---------------------------------------------------------------------------
# CONFIGURATION & PARAMETERS
# ---------------------------------------------------------------------------
PRESSURE_LEVELS = ["1000", "975", "950", "925", "900", "850", "800"]
BATCH_SIZE = 50  # Open-Meteo supports multi-location queries per request
API_TIMEOUT = 10  # Seconds before triggering fallback logic

# Build the dynamic hourly parameter request list
HOURLY_VARS = []
for level in PRESSURE_LEVELS:
    HOURLY_VARS.extend(
        [
            f"temperature_{level}hPa",
            f"relative_humidity_{level}hPa",
            f"geopotential_height_{level}hPa",
        ]
    )
HOURLY_QUERY_STR = ",".join(HOURLY_VARS)


# ---------------------------------------------------------------------------
# BATCH WEATHER INGESTION ENGINE
# ---------------------------------------------------------------------------
def fetch_production_weather_snapshot(coordinates: list[tuple[float, float]]) -> dict:
    """
    Ingests deduplicated (lat, lon) vectors, executes batched calls to Open-Meteo,
    converts thermal units to Kelvin, and returns target JSON snapshots.
    """
    # 1. Deduplicate incoming coordinates to optimize API credit usage
    unique_coords = list(set(coordinates))
    print(f"Processing {len(unique_coords)} unique coordinate vectors in batches of {BATCH_SIZE}...")

    # 2. Establish UTC snapshot target time
    current_gmt = datetime.now(timezone.utc)
    target_time_str = current_gmt.strftime("%Y-%m-%dT%H:00")
    formatted_gmt_timestamp = current_gmt.strftime("%Y-%m-%d %H:00:00 GMT")

    output_payload = {}
    
    # 3. Process coordinates in batch chunks
    for i in range(0, len(unique_coords), BATCH_SIZE):
        batch_chunk = unique_coords[i : i + BATCH_SIZE]

        # Format comma-separated lists for batch URL payload
        lats_str = ",".join([str(coord[0]) for coord in batch_chunk])
        lons_str = ",".join([str(coord[1]) for coord in batch_chunk])

        api_url = "https://api.open-meteo.com/v1/forecast"
        params = {
            "latitude": lats_str,
            "longitude": lons_str,
            "hourly": HOURLY_QUERY_STR,
            "timezone": "GMT",
            "forecast_days": 1,
        }

        try:
            # Execute batch request with SSL verification enabled
            response = requests.get(api_url, params=params, timeout=API_TIMEOUT, verify=True)
            response.raise_for_status()
            data = response.json()

            # Open-Meteo returns a list of dictionaries when querying multiple coordinates
            batch_results = data if isinstance(data, list) else [data]

            # Parse each location within the returned batch
            for coord, location_data in zip(batch_chunk, batch_results):
                coord_key = f"{coord[0]},{coord[1]}"
                hourly_data = location_data.get("hourly", {})
                times = hourly_data.get("time", [])

                # Locate current UTC hour index
                if target_time_str not in times:
                    print(f"Warning: Target hour {target_time_str} missing for {coord_key}. Skipping.")
                    continue

                target_index = times.index(target_time_str)
                levels_data = {}

                # Map isobaric levels and convert units
                for level in PRESSURE_LEVELS:
                    temp_c = hourly_data.get(f"temperature_{level}hPa", [])[target_index]
                    rh = hourly_data.get(f"relative_humidity_{level}hPa", [])[target_index]
                    geo_h = hourly_data.get(f"geopotential_height_{level}hPa", [])[target_index]

                    # Convert Celsius to Kelvin
                    temp_k = round(temp_c + 273.15, 2) if temp_c is not None else None

                    levels_data[f"{level}_hPa"] = {
                        "Atmospheric_Pressure_hPa": int(level),
                        "Air_Temperature_K": temp_k,
                        "Relative_Humidity_pct": rh,
                        "Geopotential_Height_m": geo_h,
                    }

                # Construct standardized JSON payload
                output_payload[coord_key] = {
                    "latitude": coord[0],
                    "longitude": coord[1],
                    "timestamp_gmt": formatted_gmt_timestamp,
                    "levels": levels_data,
                }

            print(f" Successfully processed batch chunk [{i + len(batch_chunk)}/{len(unique_coords)}]")

        except requests.exceptions.RequestException as e:
            # Trigger Stale-Data Fallback Warning for backend math engine
            print(f" CRITICAL: Open-Meteo API Pull Failed: {e}")
            print("Action: Triggering 3-hour stale data cache fallback & setting alert flag to 'Probable Ducting (Weather Cached)'.")
            return {"status": "FALLBACK_TRIGGERED", "error": str(e), "cached_payload": None}

        time.sleep(0.1)  # Gentle rate-limit pause between batches

    return output_payload


# ---------------------------------------------------------------------------
# EXAMPLE RUNNER (Simulating input vector list from lrf_cochannel_vectors index)
# ---------------------------------------------------------------------------
if __name__ == "__main__":
    # Example input vector list containing LRFs, Midpoints, and Transmitters
    sample_vector_coords = [
        (37.7749, -122.4194),  # San Francisco LRF
        (38.1782, -121.9569),  # SF <-> Sacramento Midpoint
        (38.5816, -121.4944),  # Sacramento Transmitter
        (46.8083, -100.7837),  # Bismarck LRF
        (47.5192, -101.0368),  # BIS <-> Minot Midpoint
        (48.2330, -101.2923),  # Minot Transmitter
    ]

    weather_snapshot = fetch_production_weather_snapshot(sample_vector_coords)

    # Save finalized output snapshot to disk
    if weather_snapshot.get("status") != "FALLBACK_TRIGGERED":
        file_ts = datetime.now(timezone.utc).strftime("%Y-%m-%d_%H-00-00_GMT")
        filename = f"production_weather_snapshot_{file_ts}.json"
        with open(filename, "w") as f:
            json.dump(weather_snapshot, f, indent=4)
        print(f"\nFinalized snapshot successfully generated and saved to '{filename}'.")