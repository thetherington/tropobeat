import csv
import time

from curl_cffi import requests

# Strictly the 50 States + DC (Removed PR, GU, VI)
states = [
    "AL", "AK", "AZ", "AR", "CA", "CO", "CT", "DE", "FL", "GA", 
    "HI", "ID", "IL", "IN", "IA", "KS", "KY", "LA", "ME", "MD", 
    "MA", "MI", "MN", "MS", "MO", "MT", "NE", "NV", "NH", "NJ", 
    "NM", "NY", "NC", "ND", "OH", "OK", "OR", "PA", "RI", "SC", 
    "SD", "TN", "TX", "UT", "VT", "VA", "WA", "WV", "WI", "WY", 
    "DC"
]

output_file = "active_full_power_tv.csv"
total_count = 0

def dms_to_dec(deg, min, sec, direction):
    """Converts Degrees, Minutes, Seconds into standard Decimal mapping coordinates."""
    try:
        dec = float(deg) + float(min)/60 + float(sec)/3600
        # South and West coordinates must be negative for GPS mapping
        if direction.upper() in ['S', 'W']:
            dec = -dec
        return f"{dec:.6f}"
    except:
        return ""

print("Starting FCC database download...")
print("Filtering for: 50 States | Licensed Only | Full-Service (DTV/TV) Only\n")

with open(output_file, 'w', newline='', encoding='utf-8') as f:
    writer = csv.writer(f)
    # Added 'Service' column so you can verify it only grabbed Full-Service DTV
    writer.writerow(['Call Sign', 'City', 'State', 'Service', 'Latitude', 'Longitude'])
    
    for state in states:
        print(f"Fetching {state}...")
        
        url = f"https://transition.fcc.gov/fcc-bin/tvq?state=%7Bstate%7D&call=&city=&arn=&serv=&vac=0&facid=&asrn=&list=4"
        
        try:
            # Bypass the firewall and SSL errors
            response = requests.get(url, impersonate="chrome", timeout=30, verify=False)
            
            if response.status_code == 200:
                lines = response.text.split('\n')
                state_count = 0
                
                for line in lines:
                    line = line.strip()
                    if not line:
                        continue
                        
                    if line.startswith('|'):
                        line = line[1:]
                        
                    cols = [c.strip() for c in line.split('|')]
                    
                    if len(cols) > 25:
                        call_sign = cols[0]
                        service = cols[2]     # Identifies if it is DTV, LPD, DCA, etc.
                        status = cols[8]
                        city = cols[9]
                        state_col = cols[10]
                        
                        # --- THE NEW FILTER ---
                        # Only grab stations that are Licensed (LIC) AND Full-Service TV (DTV or TV)
                        if status == "LIC" and service in ["DTV", "TV", "DT"]:
                            
                            # Dynamic coordinate finding
                            lat_dir_idx = -1
                            for i, c in enumerate(cols):
                                if c in ['N', 'S'] and i > 10:
                                    lat_dir_idx = i
                                    break
                                    
                            if lat_dir_idx != -1 and len(cols) > lat_dir_idx + 7:
                                lat_dir = cols[lat_dir_idx]
                                lat_deg = cols[lat_dir_idx+1]
                                lat_min = cols[lat_dir_idx+2]
                                lat_sec = cols[lat_dir_idx+3]
                                
                                lon_dir = cols[lat_dir_idx+4]
                                lon_deg = cols[lat_dir_idx+5]
                                lon_min = cols[lat_dir_idx+6]
                                lon_sec = cols[lat_dir_idx+7]
                                
                                lat_dec = dms_to_dec(lat_deg, lat_min, lat_sec, lat_dir)
                                lon_dec = dms_to_dec(lon_deg, lon_min, lon_sec, lon_dir)
                                
                                if lat_dec and lon_dec:
                                    writer.writerow([call_sign, city, state_col, service, lat_dec, lon_dec])
                                    state_count += 1
                                    total_count += 1
                                    
                print(f"  -> Found {state_count} full-service transmitters.")
                
            else:
                print(f"  -> HTTP Error {response.status_code}")
                
        except Exception as e:
            print(f"  -> Error fetching {state}: {e}")
            
        time.sleep(1.5)

print(f"\nDone! Successfully extracted {total_count} full-service transmitters to '{output_file}'")