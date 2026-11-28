package classify

import (
	"encoding/json"
	"errors"
	"math"
)

// DecodeAssessment rejects partial responses rather than interpreting missing probabilities as zero.
func DecodeAssessment(raw []byte, engine string) (SmartSignals, error) {
	var fields map[string]json.RawMessage
	var s SmartSignals
	if err := json.Unmarshal(raw, &fields); err != nil {
		return s, err
	}
	for _, key := range []string{"model", "actionable_prob", "time_sensitive_prob", "category",
		"category_prob", "category_confidence", "urgency_score", "urgency_confidence"} {
		if value := fields[key]; len(value) == 0 || string(value) == "null" {
			return s, errors.New("assessment is missing a required signal")
		}
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, err
	}
	if engine != "" {
		s.Engine = engine
	}
	if !s.ValidAssessment() {
		return s, errors.New("invalid assessment values")
	}
	return s, nil
}

// ValidAssessment checks values from independent decision providers before routing.
func (s SmartSignals) ValidAssessment() bool {
	if (s.Engine != "typesafe" && s.Engine != "local-model") || s.Model == "" || len(s.Model) > 128 || !validCategory(s.Category) {
		return false
	}
	for _, number := range []float64{s.ActionableProb, s.TimeSensitiveProb, s.CategoryProb, s.CategoryConf, s.UrgencyConf, s.UrgencyScore / 3} {
		if math.IsNaN(number) || number < 0 || number > 1 {
			return false
		}
	}
	return true
}
