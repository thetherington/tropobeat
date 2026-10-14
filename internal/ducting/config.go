package ducting

import "fmt"

// Publish modes for ducting events.
const (
	// PublishAll publishes a ducting event for every location on every poll.
	PublishAll = "all"
	// PublishNotable publishes ducting events only for super-refractive and
	// ducting locations.
	PublishNotable = "notable"
)

// Config holds the ducting settings. The classes follow ITU-R P.453 and are
// fixed; the score bands are initial values to be calibrated.
type Config struct {
	// Publish selects which locations get a ducting event: PublishAll or
	// PublishNotable.
	Publish string `config:"publish"`
	// MinLayerM is the thinnest layer kept, in metres. A level closer than
	// this to the point below it is dropped, since a tiny height difference
	// turns rounding noise into huge gradients.
	MinLayerM float64 `config:"min_layer_m"`
	// Score configures the likelihood score.
	Score ScoreConfig `config:"score"`
}

// ScoreConfig sets the bands that map the steepest refractivity gradient
// (dN/dh, N-units per km) to a 0–1 score. Gradients at or above
// SuperRefractiveNPerKm score 0; the score rises linearly to DuctingScore at
// DuctingNPerKm, then to 1 at StrongNPerKm.
type ScoreConfig struct {
	SuperRefractiveNPerKm float64 `config:"super_refractive_n_per_km"`
	DuctingNPerKm         float64 `config:"ducting_n_per_km"`
	StrongNPerKm          float64 `config:"strong_n_per_km"`
	DuctingScore          float64 `config:"ducting_score"`
}

// DefaultConfig publishes every location, drops layers thinner than 20 m and
// scores on the ITU-R P.453 class boundaries.
var DefaultConfig = Config{
	Publish:   PublishAll,
	MinLayerM: 20,
	Score: ScoreConfig{
		SuperRefractiveNPerKm: -79,
		DuctingNPerKm:         -157,
		StrongNPerKm:          -300,
		DuctingScore:          0.6,
	},
}

// Validate checks the settings. It is called when the configuration is
// unpacked.
func (c Config) Validate() error {
	if c.Publish != PublishAll && c.Publish != PublishNotable {
		return fmt.Errorf("ducting.publish must be %q or %q, got %q", PublishAll, PublishNotable, c.Publish)
	}
	if c.MinLayerM < 0 {
		return fmt.Errorf("ducting.min_layer_m must not be negative, got %v", c.MinLayerM)
	}
	s := c.Score
	if !(s.SuperRefractiveNPerKm > s.DuctingNPerKm && s.DuctingNPerKm > s.StrongNPerKm) {
		return fmt.Errorf("ducting.score: need super_refractive_n_per_km > ducting_n_per_km > strong_n_per_km, got %v, %v, %v",
			s.SuperRefractiveNPerKm, s.DuctingNPerKm, s.StrongNPerKm)
	}
	if s.DuctingScore <= 0 || s.DuctingScore >= 1 {
		return fmt.Errorf("ducting.score.ducting_score must be between 0 and 1, got %v", s.DuctingScore)
	}
	return nil
}
