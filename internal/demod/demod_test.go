package demod

import (
	"encoding/json"
	"testing"
	"time"
)

// A locked report as indexed (and sent on RabbitMQ), trimmed from the
// 2026-10-13 sample.
const lockedDoc = `{
  "@timestamp": "2026-10-13T15:11:22.778068237Z",
  "host": {"ip": "10.91.95.129"},
  "rfdemod": {"rf_demod": {
    "i_rf_channel": 21, "i_input": 3, "i_input_power": -55, "i_slot": 9,
    "s_card_label": "JACTN - Demod Card 1 - A", "s_demod_side": "A",
    "as_ids": ["150.2@s"], "d_pre_fec_ber": 0, "s_lrf_id": "jactn ",
    "s_input_status": "Locked", "s_input_tag": "WJKT (OTA)",
    "s_frame_label": "Jackson, TN - A", "s_card_type": "7880DM4-ATSC",
    "i_input_mer": 30
  }}
}`

func TestUnmarshalLocked(t *testing.T) {
	var r Report
	if err := json.Unmarshal([]byte(lockedDoc), &r); err != nil {
		t.Fatal(err)
	}
	if !r.Valid() {
		t.Fatalf("report not valid: %+v", r)
	}
	want := Instance{HostIP: "10.91.95.129", Slot: 9, Input: 3}
	if r.Instance() != want {
		t.Errorf("Instance() = %v, want %v", r.Instance(), want)
	}
	if got := r.Instance().String(); got != "10.91.95.129/9/3" {
		t.Errorf("Instance().String() = %q", got)
	}
	if r.Channel() != (ChannelKey{LRF: "JACTN", RFChannel: 21}) {
		t.Errorf("Channel() = %v", r.Channel())
	}
	if !r.Locked() || r.Saturated() {
		t.Errorf("Locked=%v Saturated=%v", r.Locked(), r.Saturated())
	}
	if r.MER != 30 || r.PowerDBm != -55 || r.BER != 0 || r.Side != "A" || r.Tag != "WJKT (OTA)" {
		t.Errorf("fields = %+v", r)
	}
	if want := time.Date(2026, 10, 13, 15, 11, 22, 778068237, time.UTC); !r.Timestamp.Equal(want) {
		t.Errorf("Timestamp = %v", r.Timestamp)
	}
}

func TestUnmarshalUnlockedAndSaturated(t *testing.T) {
	doc := `{"@timestamp":"2026-10-13T15:00:00Z","host":{"ip":"10.0.0.1"},
	  "rfdemod":{"rf_demod":{"i_slot":0,"i_input":0,"i_rf_channel":37,"s_lrf_id":"GBAY",
	  "s_demod_side":"1 A","s_input_status":"Locked","d_pre_fec_ber":16.777215,"i_input_mer":0,"i_input_power":-80}}}`
	var r Report
	if err := json.Unmarshal([]byte(doc), &r); err != nil {
		t.Fatal(err)
	}
	// Slot 0 and input 0 are valid numbers, not missing values.
	if !r.Valid() {
		t.Fatalf("report with slot 0 / input 0 should be valid: %+v", r)
	}
	if !r.Saturated() {
		t.Error("BER 16.777215 should be saturated")
	}
	if r.Side != "A" {
		t.Errorf(`side "1 A" normalised to %q, want "A"`, r.Side)
	}
}

func TestValidRejectsIncomplete(t *testing.T) {
	cases := map[string]string{
		"no host ip":   `{"@timestamp":"2026-10-13T15:00:00Z","rfdemod":{"rf_demod":{"i_slot":9,"i_input":1,"i_rf_channel":10,"s_lrf_id":"TERH"}}}`,
		"no slot":      `{"@timestamp":"2026-10-13T15:00:00Z","host":{"ip":"10.0.0.1"},"rfdemod":{"rf_demod":{"i_input":1,"i_rf_channel":10,"s_lrf_id":"TERH"}}}`,
		"no input":     `{"@timestamp":"2026-10-13T15:00:00Z","host":{"ip":"10.0.0.1"},"rfdemod":{"rf_demod":{"i_slot":9,"i_rf_channel":10,"s_lrf_id":"TERH"}}}`,
		"no lrf":       `{"@timestamp":"2026-10-13T15:00:00Z","host":{"ip":"10.0.0.1"},"rfdemod":{"rf_demod":{"i_slot":9,"i_input":1,"i_rf_channel":10}}}`,
		"no channel":   `{"@timestamp":"2026-10-13T15:00:00Z","host":{"ip":"10.0.0.1"},"rfdemod":{"rf_demod":{"i_slot":9,"i_input":1,"s_lrf_id":"TERH"}}}`,
		"no timestamp": `{"host":{"ip":"10.0.0.1"},"rfdemod":{"rf_demod":{"i_slot":9,"i_input":1,"i_rf_channel":10,"s_lrf_id":"TERH"}}}`,
	}
	for name, doc := range cases {
		var r Report
		if err := json.Unmarshal([]byte(doc), &r); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if r.Valid() {
			t.Errorf("%s: Valid() = true", name)
		}
	}
	var r Report
	if err := json.Unmarshal([]byte(`not json`), &r); err == nil {
		t.Error("malformed JSON: no error")
	}
}

func TestNormalizeSide(t *testing.T) {
	for in, want := range map[string]string{"A": "A", " b ": "B", "1 A": "A", "2 B": "B", "": "", "C": "C"} {
		if got := NormalizeSide(in); got != want {
			t.Errorf("NormalizeSide(%q) = %q, want %q", in, got, want)
		}
	}
}
