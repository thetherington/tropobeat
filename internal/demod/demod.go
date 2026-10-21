// Package demod models the RF demod telemetry that inSITE collects from each
// LRF: one report per demod input about once a minute, with lock status,
// MER, Pre-FEC BER and input power. The same JSON is indexed in the daily
// log-metric-p-rfdemod-* indices and sent on the RabbitMQ demod stream.
package demod

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Demod telemetry fields, as indexed. The rfdemod.rf_demod fields are
// keyword, integer or double; host.ip is an ip.
const (
	FieldTimestamp   = "@timestamp"
	FieldHostIP      = "host.ip"
	FieldLRFID       = "rfdemod.rf_demod.s_lrf_id"
	FieldSide        = "rfdemod.rf_demod.s_demod_side"
	FieldSlot        = "rfdemod.rf_demod.i_slot"
	FieldInput       = "rfdemod.rf_demod.i_input"
	FieldRFChannel   = "rfdemod.rf_demod.i_rf_channel"
	FieldInputTag    = "rfdemod.rf_demod.s_input_tag"
	FieldInputStatus = "rfdemod.rf_demod.s_input_status"
	FieldCardLabel   = "rfdemod.rf_demod.s_card_label"
	FieldFrameLabel  = "rfdemod.rf_demod.s_frame_label"
	FieldMER         = "rfdemod.rf_demod.i_input_mer"
	FieldPower       = "rfdemod.rf_demod.i_input_power"
	FieldPreFECBER   = "rfdemod.rf_demod.d_pre_fec_ber"
)

// StatusLocked is the s_input_status of an input that is locked to its
// signal. Unlocked inputs report "Not Locked", with MER 0.
const StatusLocked = "Locked"

// DefaultIndex is the default index pattern for demod telemetry, which is
// stored in daily indices such as log-metric-p-rfdemod-2026.10.13.
const DefaultIndex = "log-metric-p-rfdemod-*"

// BERSaturated is the Pre-FEC BER a demod reports when the reading is
// saturated or invalid: 16.777215, i.e. 0xFFFFFF / 1e6. Any BER at or above
// it is treated as saturated rather than as a value.
const BERSaturated = 16.777215

// Instance identifies one demod input at an LRF: the demod frame's host IP,
// the card slot and the input on the card. It stays the same when the
// input's LRF label, tag or channel are edited.
type Instance struct {
	HostIP string
	Slot   int
	Input  int
}

// String returns the instance as "<host ip>/<slot>/<input>", e.g.
// "10.91.114.129/9/2".
func (i Instance) String() string {
	return fmt.Sprintf("%s/%d/%d", i.HostIP, i.Slot, i.Input)
}

// ChannelKey groups the demod instances at one LRF that receive the same RF
// channel, e.g. the side A and side B inputs of one station.
type ChannelKey struct {
	LRF       string
	RFChannel int
}

// Report is one demod input report.
type Report struct {
	Timestamp time.Time
	HostIP    string
	// LRF is the s_lrf_id, upper-cased and trimmed.
	LRF string
	// Side is the demod side, "A" or "B", normalised (see NormalizeSide).
	// Empty when the report has none.
	Side       string
	Slot       int
	Input      int
	RFChannel  int
	Tag        string
	Status     string
	CardLabel  string
	FrameLabel string
	MER        int
	PowerDBm   int
	BER        float64

	// hasSlot, hasInput and hasChannel record whether the integer fields
	// were present, since 0 is a valid slot or input number.
	hasSlot, hasInput, hasChannel bool
}

// rawReport is the JSON shape of a demod report: the indexed document's
// _source, which is also the RabbitMQ message body. Pointers tell missing
// fields from zero values.
type rawReport struct {
	Timestamp time.Time `json:"@timestamp"`
	Host      struct {
		IP string `json:"ip"`
	} `json:"host"`
	RFDemod struct {
		RFDemod struct {
			LRFID       string   `json:"s_lrf_id"`
			Side        string   `json:"s_demod_side"`
			Slot        *int     `json:"i_slot"`
			Input       *int     `json:"i_input"`
			RFChannel   *int     `json:"i_rf_channel"`
			InputTag    string   `json:"s_input_tag"`
			InputStatus string   `json:"s_input_status"`
			CardLabel   string   `json:"s_card_label"`
			FrameLabel  string   `json:"s_frame_label"`
			MER         *int     `json:"i_input_mer"`
			Power       *int     `json:"i_input_power"`
			PreFECBER   *float64 `json:"d_pre_fec_ber"`
		} `json:"rf_demod"`
	} `json:"rfdemod"`
}

// UnmarshalJSON decodes a demod report from the indexed document shape,
// normalising the LRF ID, side and tag. It does not check that the report is
// complete; see Valid.
func (r *Report) UnmarshalJSON(b []byte) error {
	var raw rawReport
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	d := raw.RFDemod.RFDemod
	*r = Report{
		Timestamp:  raw.Timestamp,
		HostIP:     strings.TrimSpace(raw.Host.IP),
		LRF:        NormalizeLRF(d.LRFID),
		Side:       NormalizeSide(d.Side),
		Tag:        d.InputTag,
		Status:     d.InputStatus,
		CardLabel:  d.CardLabel,
		FrameLabel: d.FrameLabel,
	}
	if d.Slot != nil {
		r.Slot, r.hasSlot = *d.Slot, true
	}
	if d.Input != nil {
		r.Input, r.hasInput = *d.Input, true
	}
	if d.RFChannel != nil {
		r.RFChannel, r.hasChannel = *d.RFChannel, true
	}
	if d.MER != nil {
		r.MER = *d.MER
	}
	if d.Power != nil {
		r.PowerDBm = *d.Power
	}
	if d.PreFECBER != nil {
		r.BER = *d.PreFECBER
	}
	return nil
}

// Valid reports whether the report can be used: it has a timestamp, an
// instance (host IP, slot and input), an LRF and an RF channel.
func (r Report) Valid() bool {
	return !r.Timestamp.IsZero() && r.HostIP != "" && r.hasSlot && r.hasInput && r.LRF != "" && r.hasChannel
}

// Instance returns the demod input the report came from.
func (r Report) Instance() Instance {
	return Instance{HostIP: r.HostIP, Slot: r.Slot, Input: r.Input}
}

// Channel returns the LRF and RF channel the report is for.
func (r Report) Channel() ChannelKey {
	return ChannelKey{LRF: r.LRF, RFChannel: r.RFChannel}
}

// Locked reports whether the input is locked to its signal.
func (r Report) Locked() bool { return r.Status == StatusLocked }

// Saturated reports whether the Pre-FEC BER is the saturated reading
// (BERSaturated) rather than a measurement.
func (r Report) Saturated() bool { return IsSaturatedBER(r.BER) }

// IsSaturatedBER reports whether ber is the saturated reading. A small
// tolerance allows for the value's decimal representation.
func IsSaturatedBER(ber float64) bool { return ber >= BERSaturated-1e-6 }

// NormalizeLRF upper-cases and trims a demod LRF ID, so it matches the LRF
// codes however the demod was labelled.
func NormalizeLRF(id string) string {
	return strings.ToUpper(strings.TrimSpace(id))
}

// NormalizeSide returns the demod side as "A" or "B". Labels such as "1 A"
// (seen in real data) keep their final letter. Anything else is returned
// upper-cased and trimmed, and an empty side stays empty.
func NormalizeSide(side string) string {
	s := strings.ToUpper(strings.TrimSpace(side))
	if f := strings.Fields(s); len(f) > 1 {
		if last := f[len(f)-1]; last == "A" || last == "B" {
			return last
		}
	}
	return s
}
