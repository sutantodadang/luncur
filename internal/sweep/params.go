// Package sweep provides the pieces of hyperparameter-sweep support that
// have no server/store/kube dependency: parsing a params.yaml search space,
// expanding it into concrete trial parameter sets (grid or random), and
// reading a trial's reported metric (log-line contract or MLflow REST).
package sweep

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strconv"

	"sigs.k8s.io/yaml"
)

// Param is one axis of a hyperparameter search space: either a discrete
// Choices list (rendered verbatim into env) or a continuous [Min,Max] range
// (Log samples uniform in log10 space; Min>0 is required when Log is set).
type Param struct {
	Choices  []string
	Min, Max float64
	Log      bool
}

// ParseParams parses a params.yaml mapping. Each value is either a list
// (`key: [a, b, c]`, discrete) or an object (`key: {min: 1e-5, max: 1e-2,
// log: true}`, continuous). Errors name the offending key.
func ParseParams(b []byte) (map[string]Param, error) {
	var raw map[string]any
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse params.yaml: %w", err)
	}
	if len(raw) == 0 {
		return nil, errors.New("params.yaml: no parameters defined")
	}

	out := make(map[string]Param, len(raw))
	for key, v := range raw {
		p, err := parseParam(key, v)
		if err != nil {
			return nil, err
		}
		out[key] = p
	}
	return out, nil
}

func badParamErr(key string) error {
	return fmt.Errorf("param %q: want a list or {min,max[,log]}", key)
}

func parseParam(key string, v any) (Param, error) {
	switch val := v.(type) {
	case []any:
		if len(val) == 0 {
			return Param{}, badParamErr(key)
		}
		choices := make([]string, len(val))
		for i, item := range val {
			choices[i] = fmt.Sprintf("%v", item)
		}
		return Param{Choices: choices}, nil

	case map[string]any:
		minRaw, minOK := val["min"]
		maxRaw, maxOK := val["max"]
		if !minOK || !maxOK {
			return Param{}, badParamErr(key)
		}
		minF, ok := minRaw.(float64)
		if !ok {
			return Param{}, badParamErr(key)
		}
		maxF, ok := maxRaw.(float64)
		if !ok {
			return Param{}, badParamErr(key)
		}
		logFlag := false
		if lv, ok := val["log"]; ok {
			b, ok := lv.(bool)
			if !ok {
				return Param{}, badParamErr(key)
			}
			logFlag = b
		}
		if minF >= maxF {
			return Param{}, fmt.Errorf("param %q: min must be less than max", key)
		}
		if logFlag && minF <= 0 {
			return Param{}, fmt.Errorf("param %q: log scale requires min > 0", key)
		}
		return Param{Min: minF, Max: maxF, Log: logFlag}, nil

	default:
		return Param{}, badParamErr(key)
	}
}

// Expand produces the trial param sets as env-ready string maps.
// All-discrete spaces produce a deterministic grid (sorted-key mixed-radix
// order, last key varies fastest), truncated to maxTrials with the second
// return set true when truncation happened. Any continuous axis produces
// exactly maxTrials random samples drawn via rng (never truncated).
func Expand(space map[string]Param, maxTrials int, rng *rand.Rand) ([]map[string]string, bool, error) {
	if len(space) == 0 {
		return nil, false, errors.New("empty param space")
	}
	if maxTrials < 1 {
		return nil, false, errors.New("maxTrials must be >= 1")
	}

	keys := make([]string, 0, len(space))
	for k := range space {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	allDiscrete := true
	for _, k := range keys {
		if len(space[k].Choices) == 0 {
			allDiscrete = false
			break
		}
	}

	if allDiscrete {
		sets, truncated := expandGrid(space, keys, maxTrials)
		return sets, truncated, nil
	}
	return expandRandom(space, keys, maxTrials, rng), false, nil
}

func expandGrid(space map[string]Param, keys []string, maxTrials int) ([]map[string]string, bool) {
	total := 1
	for _, k := range keys {
		total *= len(space[k].Choices)
	}
	n := total
	truncated := false
	if n > maxTrials {
		n = maxTrials
		truncated = true
	}

	out := make([]map[string]string, 0, n)
	counters := make([]int, len(keys))
	for i := 0; i < n; i++ {
		set := make(map[string]string, len(keys))
		for ki, k := range keys {
			set[k] = space[k].Choices[counters[ki]]
		}
		out = append(out, set)

		// Odometer increment: last key advances fastest.
		for ki := len(keys) - 1; ki >= 0; ki-- {
			counters[ki]++
			if counters[ki] < len(space[keys[ki]].Choices) {
				break
			}
			counters[ki] = 0
		}
	}
	return out, truncated
}

func expandRandom(space map[string]Param, keys []string, maxTrials int, rng *rand.Rand) []map[string]string {
	out := make([]map[string]string, 0, maxTrials)
	for i := 0; i < maxTrials; i++ {
		set := make(map[string]string, len(keys))
		for _, k := range keys {
			p := space[k]
			if len(p.Choices) > 0 {
				set[k] = p.Choices[rng.Intn(len(p.Choices))]
				continue
			}
			var v float64
			if p.Log {
				lo, hi := math.Log10(p.Min), math.Log10(p.Max)
				v = math.Pow(10, lo+rng.Float64()*(hi-lo))
			} else {
				v = p.Min + rng.Float64()*(p.Max-p.Min)
			}
			set[k] = strconv.FormatFloat(v, 'g', -1, 64)
		}
		out = append(out, set)
	}
	return out
}
