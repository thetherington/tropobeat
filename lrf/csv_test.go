package lrf

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	// Latin-1 encoded degree sign (0xB0) and CRLF line endings, as exported by Excel.
	input := "DMA Name,Code,Physical Street Address,Physical City,State,Zip Code,Physical City Lattitude and Longitude,Physical City  GPS coordinates\r\n" +
		"Abilene TX,ABL,409 Baird St,Abilene,TX,79602,\"32.4449556, -99.7141538\",\"32\xb0 26' 41.84\"\" N, 99\xb0 42' 50.95\"\" W\"\r\n" +
		"Bangor ME,BAN,329 Mount Hope Ave,Bangor,ME,4401,\" 44.8169088, -68.7520044\",\r\n"

	got, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d records, want 2", len(got))
	}

	want := LRF{
		DMAName:   "Abilene TX",
		Code:      "ABL",
		Street:    "409 Baird St",
		City:      "Abilene",
		State:     "TX",
		ZipCode:   "79602",
		Latitude:  32.4449556,
		Longitude: -99.7141538,
		GPSDMS:    `32° 26' 41.84" N, 99° 42' 50.95" W`,
	}
	if got[0] != want {
		t.Errorf("record 0:\n got %+v\nwant %+v", got[0], want)
	}

	if got[1].ZipCode != "04401" {
		t.Errorf("zip: got %q, want %q", got[1].ZipCode, "04401")
	}
	if got[1].Latitude != 44.8169088 {
		t.Errorf("latitude: got %v, want %v", got[1].Latitude, 44.8169088)
	}
}

func TestParseMissingColumn(t *testing.T) {
	_, err := Parse(strings.NewReader("DMA Name,Code\r\nAbilene TX,ABL\r\n"))
	if err == nil {
		t.Fatal("expected error for missing columns")
	}
}

func TestParseBadCoordinates(t *testing.T) {
	input := "DMA Name,Code,Physical Street Address,Physical City,State,Zip Code,Physical City Lattitude and Longitude\n" +
		"Abilene TX,ABL,409 Baird St,Abilene,TX,79602,not-a-coordinate\n"
	if _, err := Parse(strings.NewReader(input)); err == nil {
		t.Fatal("expected error for invalid coordinates")
	}
}
