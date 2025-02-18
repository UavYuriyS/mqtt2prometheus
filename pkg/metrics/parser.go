package metrics

import (
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
	"github.com/hikhvar/mqtt2prometheus/pkg/config"
	"gopkg.in/yaml.v2"
)

// dynamicState holds the runtime information for dynamic metric configs.
type dynamicState struct {
	// Basline value to add to each parsed metric value to maintain monotonicy
	Offset float64 `yaml:"value_offset"`
	// Last value that was parsed before the offset was added
	LastRawValue float64 `yaml:"last_raw_value"`
	// Last value that was used for evaluating the given expression
	LastExprValue float64 `yaml:"last_expr_value"`
	// Last result returned from evaluating the given expression
	LastExprRawValue interface{} `yaml:"last_expr_raw_value"`
	// Last result returned from evaluating the given expression
	LastExprResult float64 `yaml:"last_expr_result"`
	// Last result (String) returned from evaluating the given expression
	LastExprResultString string `yaml:"last_expr_result_string"`
	// Last result returned from evaluating the given expression
	LastExprTimestamp time.Time `yaml:"last_expr_timestamp"`
}

// metricState holds runtime information per metric configuration.
type metricState struct {
	dynamic dynamicState
	// The last time the state file was written
	lastWritten time.Time
	// Compiled evaluation expression
	program *vm.Program
	// Environment in which the expression is evaluated
	env map[string]interface{}
}

type Parser struct {
	separator string
	// Maps the mqtt metric name to a list of configs
	// The first that matches SensorNameFilter will be used
	metricConfigs map[string][]*config.MetricConfig
	// Directory holding state files
	stateDir string
	// Per-metric state
	states map[string]*metricState
}

// Identifiers within the expression evaluation environment.
const (
	env_raw_value      = "raw_value"
	env_value          = "value"
	env_last_value     = "last_value"
	env_last_raw_value = "last_raw_value"
	env_last_result    = "last_result"
	env_elapsed        = "elapsed"
	env_now            = "now"
	env_int            = "int"
	env_float          = "float"
	env_round          = "round"
	env_ceil           = "ceil"
	env_floor          = "floor"
	env_abs            = "abs"
	env_min            = "min"
	env_max            = "max"
)

var now = time.Now

func toInt64(i interface{}) int64 {
	switch v := i.(type) {
	case float32:
		return int64(v)
	case float64:
		return int64(v)
	case int:
		return int64(v)
	case int32:
		return int64(v)
	case int64:
		return v
	case time.Duration:
		return int64(v)
	case string:
		value, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			panic(err)
		}
		return value
	default:
		return v.(int64) // Hope for the best
	}
}

func toFloat64(i interface{}) float64 {
	switch v := i.(type) {
	case float32:
		return float64(v)
	case float64:
		return v
	case int:
		return float64(v)
	case int32:
		return float64(v)
	case int64:
		return float64(v)
	case time.Duration:
		return float64(v)
	case string:
		value, err := strconv.ParseFloat(v, 64)
		if err != nil {
			panic(err)
		}
		return value
	default:
		return v.(float64) // Hope for the best
	}
}

// defaultExprEnv returns the default environment for expression evaluation.
func defaultExprEnv() map[string]interface{} {
	return map[string]interface{}{
		// Variables
		env_raw_value:   nil,
		env_value:       0.0,
		env_last_value:  0.0,
		env_last_result: 0.0,
		env_elapsed:     time.Duration(0),
		// Functions
		env_now:   now,
		env_int:   toInt64,
		env_float: toFloat64,
		env_round: math.Round,
		env_ceil:  math.Ceil,
		env_floor: math.Floor,
		env_abs:   math.Abs,
		env_min:   math.Min,
		env_max:   math.Max,
	}
}

func NewParser(metric []config.BlockConfig, separator, stateDir string) Parser {
	cfgs := make(map[string][]*config.MetricConfig)
	for _, metrics := range metric {
		for i := range metrics.Metrics {
			key := metrics.Metrics[i].MQTTName
			cfgs[key] = append(cfgs[key], &metrics.Metrics[i])
		}
	}
	return Parser{
		separator:     separator,
		metricConfigs: cfgs,
		stateDir:      strings.TrimRight(stateDir, "/"),
		states:        make(map[string]*metricState),
	}
}

// Config returns the underlying metrics config
func (p *Parser) config() map[string][]*config.MetricConfig {
	return p.metricConfigs
}

// validMetric returns all configs matching the metric and deviceID.
func (p *Parser) findMetricConfigs(metric string, deviceID string) []*config.MetricConfig {
	configs := []*config.MetricConfig{}
	for _, c := range p.metricConfigs[metric] {
		if c.SensorNameFilter.Match(deviceID) {
			configs = append(configs, c)
		}
	}
	return configs
}

// parseMetric parses the given value according to the given deviceID and metricPath. The config allows to
// parse a metric value according to the device ID.
func (p *Parser) parseMetric(cfg *config.MetricConfig, metricID string, value interface{}) (Metric, error) {
	var metricValue float64
	var err error

	if cfg.RawExpression != "" {
		if metricValue, err = p.evalExpressionValue(metricID, cfg.RawExpression, value, metricValue); err != nil {
			if cfg.ErrorValue != nil {
				metricValue = *cfg.ErrorValue
			} else {
				return Metric{}, err
			}
		}
	} else {

		if boolValue, ok := value.(bool); ok {
			if boolValue {
				metricValue = 1
			} else {
				metricValue = 0
			}
		} else if strValue, ok := value.(string); ok {

			// If string value mapping is defined, use that
			if cfg.StringValueMapping != nil {

				floatValue, ok := cfg.StringValueMapping.Map[strValue]
				if ok {
					metricValue = floatValue

					// deprecated, replaced by ErrorValue from the upper level
				} else if cfg.StringValueMapping.ErrorValue != nil {
					metricValue = *cfg.StringValueMapping.ErrorValue
				} else if cfg.ErrorValue != nil {
					metricValue = *cfg.ErrorValue
				} else {
					return Metric{}, fmt.Errorf("got unexpected string data '%s'", strValue)
				}

			} else {

				// otherwise try to parse float
				floatValue, err := strconv.ParseFloat(strValue, 64)
				if err != nil {
					if cfg.ErrorValue != nil {
						metricValue = *cfg.ErrorValue
					} else {
						return Metric{}, fmt.Errorf("got data with unexpectd type: %T ('%v') and failed to parse to float", value, value)
					}
				} else {
					metricValue = floatValue
				}

			}

		} else if floatValue, ok := value.(float64); ok {
			metricValue = floatValue
		} else if cfg.ErrorValue != nil {
			metricValue = *cfg.ErrorValue
		} else {
			return Metric{}, fmt.Errorf("got data with unexpectd type: %T ('%v')", value, value)
		}

		if cfg.Expression != "" {
			if metricValue, err = p.evalExpressionValue(metricID, cfg.Expression, value, metricValue); err != nil {
				if cfg.ErrorValue != nil {
					metricValue = *cfg.ErrorValue
				} else {
					return Metric{}, err
				}
			}
		}
	}

	if cfg.ForceMonotonicy {
		if metricValue, err = p.enforceMonotonicy(metricID, metricValue); err != nil {
			if cfg.ErrorValue != nil {
				metricValue = *cfg.ErrorValue
			} else {
				return Metric{}, err
			}
		}
	}

	if cfg.MQTTValueScale != 0 {
		metricValue = metricValue * cfg.MQTTValueScale
	}

	var ingestTime time.Time
	if !cfg.OmitTimestamp {
		ingestTime = now()
	}

	// generate dynamic labels
	var labels map[string]string
	if len(cfg.DynamicLabels) > 0 {
		labels = make(map[string]string, len(cfg.DynamicLabels))
		for k, v := range cfg.DynamicLabels {
			value, err := p.evalExpressionLabel(metricID, k, v, value, metricValue)
			if err != nil {
				return Metric{}, err
			}
			labels[k] = value
		}
	}

	return Metric{
		Description: cfg.PrometheusDescription(),
		Value:       metricValue,
		ValueType:   cfg.PrometheusValueType(),
		IngestTime:  ingestTime,
		Labels:      labels,
		LabelsKeys:  cfg.DynamicLabelsKeys(),
	}, nil
}

func (p *Parser) stateFileName(metricID string) string {
	return fmt.Sprintf("%s/%s.yaml", p.stateDir, metricID)
}

// readMetricState parses the metric state from the configured path.
// If the file does not exist, an empty state is returned.
func (p *Parser) readMetricState(metricID string) (*metricState, error) {
	state := &metricState{}
	f, err := os.Open(p.stateFileName(metricID))
	if err != nil {
		// The file does not exist for new metrics.
		if os.IsNotExist(err) {
			return state, nil
		}
		return state, fmt.Errorf("failed to read file %q: %v", f.Name(), err)
	}
	defer f.Close()

	var data []byte
	if info, err := f.Stat(); err == nil {
		data = make([]byte, int(info.Size()))
	}
	if _, err := f.Read(data); err != nil && err != io.EOF {
		return state, err
	}

	err = yaml.UnmarshalStrict(data, &state.dynamic)
	state.lastWritten = now()
	return state, err
}

// writeMetricState writes back the metric's current state to the configured path.
func (p *Parser) writeMetricState(metricID string, state *metricState) error {
	out, err := yaml.Marshal(state.dynamic)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p.stateFileName(metricID), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.Write(out); err != nil {
		return fmt.Errorf("failed to write file %q: %v", f.Name(), err)
	}
	return nil
}

// getMetricState returns the state of the given metric.
// The state is read from and written back to disk as needed.
func (p *Parser) getMetricState(metricID string) (*metricState, error) {
	var err error
	state, found := p.states[metricID]
	if !found {
		if state, err = p.readMetricState(metricID); err != nil {
			return nil, err
		}
		p.states[metricID] = state
	}
	// Write the state back to disc every minute.
	if now().Sub(state.lastWritten) >= time.Minute {
		if err = p.writeMetricState(metricID, state); err == nil {
			state.lastWritten = now()
		}
	}
	return state, err
}

// enforceMonotonicy makes sure the given values never decrease from one call to the next.
// If the current value is smaller than the last one, a consistent offset is added.
func (p *Parser) enforceMonotonicy(metricID string, value float64) (float64, error) {
	ms, err := p.getMetricState(metricID)
	if err != nil {
		return value, err
	}
	// When the source metric is reset, the last adjusted value becomes the new offset.
	if value < ms.dynamic.LastRawValue {
		ms.dynamic.Offset += ms.dynamic.LastRawValue
		// Trigger flushing the new state to disk.
		ms.lastWritten = time.Time{}
	}

	ms.dynamic.LastRawValue = value
	return value + ms.dynamic.Offset, nil
}

// evalExpressionValue runs the given code in the metric's environment and returns the result.
// In case of an error, the original value is returned.
func (p *Parser) evalExpressionValue(metricID, code string, raw_value interface{}, value float64) (float64, error) {
	ms, err := p.getMetricState(metricID)
	if err != nil {
		return value, err
	}
	if ms.program == nil {
		ms.env = defaultExprEnv()
		// Update the environment
		ms.env[env_raw_value] = raw_value
		ms.env[env_value] = value
		ms.env[env_last_value] = ms.dynamic.LastExprValue
		ms.env[env_last_raw_value] = ms.dynamic.LastExprRawValue
		ms.env[env_last_result] = ms.dynamic.LastExprResult
		if ms.dynamic.LastExprTimestamp.IsZero() {
			ms.env[env_elapsed] = time.Duration(0)
		} else {
			ms.env[env_elapsed] = now().Sub(ms.dynamic.LastExprTimestamp)
		}
		ms.program, err = expr.Compile(code, expr.Env(ms.env), expr.AsFloat64())
		if err != nil {
			return value, fmt.Errorf("failed to compile expression %q: %w", code, err)
		}
		// Trigger flushing the new state to disk.
		ms.lastWritten = time.Time{}
	}

	result, err := expr.Run(ms.program, ms.env)
	if err != nil {
		return value, fmt.Errorf("failed to evaluate expression %q: %w", code, err)
	}
	// Type was statically checked above.
	ret := result.(float64)

	// Update the dynamic state
	ms.dynamic.LastExprResult = ret
	ms.dynamic.LastExprRawValue = raw_value
	ms.dynamic.LastExprValue = value
	ms.dynamic.LastExprTimestamp = now()

	return ret, nil
}

// evalExpressionLabel runs the given code in the metric's environment and returns the result.
// In case of an error, the original value is returned.
func (p *Parser) evalExpressionLabel(metricID, label, code string, rawValue interface{}, value float64) (string, error) {
	ms, err := p.getMetricState(label + "@" + metricID)
	if err != nil {
		return "", err
	}
	if ms.program == nil {
		ms.env = defaultExprEnv()
		ms.program, err = expr.Compile(code, expr.Env(ms.env))
		if err != nil {
			return "", fmt.Errorf("failed to compile dynamic label expression %q: %w", code, err)
		}
		// Trigger flushing the new state to disk.
		ms.lastWritten = time.Time{}
	}

	// Update the environment
	ms.env[env_raw_value] = rawValue
	ms.env[env_value] = value
	ms.env[env_last_value] = ms.dynamic.LastExprValue
	ms.env[env_last_raw_value] = ms.dynamic.LastExprRawValue
	ms.env[env_last_result] = ms.dynamic.LastExprResultString
	if ms.dynamic.LastExprTimestamp.IsZero() {
		ms.env[env_elapsed] = time.Duration(0)
	} else {
		ms.env[env_elapsed] = now().Sub(ms.dynamic.LastExprTimestamp)
	}

	result, err := expr.Run(ms.program, ms.env)
	if err != nil {
		return "", fmt.Errorf("failed to evaluate dynamic label expression %q: %w", code, err)
	}

	// convert to string
	ret := fmt.Sprint(result)

	// Update the dynamic state
	ms.dynamic.LastExprResultString = ret
	ms.dynamic.LastExprValue = value
	ms.dynamic.LastExprTimestamp = now()

	return ret, nil
}
