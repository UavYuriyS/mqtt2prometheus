package config

import (
	"fmt"
	"io/ioutil"
	"os"
	"reflect"
	"regexp"
	"sort"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"gopkg.in/yaml.v2"
)

const (
	GaugeValueType   = "gauge"
	CounterValueType = "counter"

	DeviceIDRegexGroup   = "deviceid"
	MetricNameRegexGroup = "metricname"
)

var MetricConfigDefaults = MetricConfig{
	TopicPathFilter: MustNewRegexp(".*"),
}

var MQTTConfigDefaults = MQTTConfig{
	Server:        "tcp://127.0.0.1:1883",
	TopicPath:     "v1/devices/me",
	DeviceIDRegex: MustNewRegexp(fmt.Sprintf("(.*/)?(?P<%s>.*)", DeviceIDRegexGroup)),
	QoS:           0,
}

var CacheConfigDefaults = CacheConfig{
	Timeout:  2 * time.Minute,
	StateDir: "/var/lib/mqtt2prometheus",
}

var JsonParsingConfigDefaults = JsonParsingConfig{
	Separator: ".",
}

type Regexp struct {
	r       *regexp.Regexp
	pattern string
}

func (rf *Regexp) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var pattern string
	if err := unmarshal(&pattern); err != nil {
		return err
	}
	r, err := regexp.Compile(pattern)
	if err != nil {
		return err
	}
	rf.r = r
	rf.pattern = pattern
	return nil
}

func (rf *Regexp) MarshalYAML() (interface{}, error) {
	if rf == nil {
		return "", nil
	}
	return rf.pattern, nil
}

func (rf *Regexp) Match(s string) bool {
	return rf == nil || rf.r == nil || rf.r.MatchString(s)
}

// GroupValue returns the value of the given group. If the group is not part of the underlying regexp, returns the empty string.
func (rf *Regexp) GroupValue(s string, groupName string) string {
	match := rf.r.FindStringSubmatch(s)
	groupValues := make(map[string]string)
	for i, name := range rf.r.SubexpNames() {
		if len(match) > i && name != "" {
			groupValues[name] = match[i]
		}
	}
	return groupValues[groupName]
}

func (rf *Regexp) RegEx() *regexp.Regexp {
	return rf.r
}

func MustNewRegexp(pattern string) *Regexp {
	return &Regexp{
		pattern: pattern,
		r:       regexp.MustCompile(pattern),
	}
}

type Config struct {
	JsonParsing     *JsonParsingConfig `yaml:"json_parsing,omitempty"`
	Metrics         []BlockConfig      `yaml:"metrics"`
	MQTT            *MQTTConfig        `yaml:"mqtt,omitempty"`
	Cache           *CacheConfig       `yaml:"cache,omitempty"`
	EnableProfiling bool               `yaml:"enable_profiling_metrics,omitempty"`
}

type CacheConfig struct {
	Timeout  time.Duration `yaml:"timeout"`
	StateDir string        `yaml:"state_directory"`
}

type JsonParsingConfig struct {
	Separator string `yaml:"separator"`
}

type MQTTConfig struct {
	Server               string                `yaml:"server"`
	TopicPath            string                `yaml:"topic_path"`
	DeviceIDRegex        *Regexp               `yaml:"device_id_regex"`
	User                 string                `yaml:"user"`
	Password             string                `yaml:"password"`
	QoS                  byte                  `yaml:"qos"`
	ObjectPerTopicConfig *ObjectPerTopicConfig `yaml:"object_per_topic_config"`
	MetricPerTopicConfig *MetricPerTopicConfig `yaml:"metric_per_topic_config"`
	CACert               string                `yaml:"ca_cert"`
	ClientCert           string                `yaml:"client_cert"`
	ClientKey            string                `yaml:"client_key"`
	ClientID             string                `yaml:"client_id"`
}

const EncodingJSON = "JSON"

type ObjectPerTopicConfig struct {
	Encoding string `yaml:"encoding"` // Currently only JSON is a valid value
}

type MetricPerTopicConfig struct {
	MetricNameRegex *Regexp `yaml:"metric_name_regex"` // Default
}

// Metrics Config is a mapping between a metric send on mqtt to a prometheus metric

type MetricConfig struct {
	PrometheusName     string                    `yaml:"prom_name"`
	MQTTName           string                    `yaml:"mqtt_name"`
	PayloadField       string                    `yaml:"payload_field"`
	SensorNameFilter   Regexp                    `yaml:"sensor_name_filter"`
	TopicPathFilter    *Regexp                   `yaml:"topic_path_filter"`
	Help               string                    `yaml:"help"`
	ValueType          string                    `yaml:"type"`
	OmitTimestamp      bool                      `yaml:"omit_timestamp"`
	RawExpression      string                    `yaml:"raw_expression"`
	Expression         string                    `yaml:"expression"`
	ForceMonotonicy    bool                      `yaml:"force_monotonicy"`
	ConstantLabels     map[string]string         `yaml:"const_labels"`
	DynamicLabels      map[string]string         `yaml:"dynamic_labels"`
	StringValueMapping *StringValueMappingConfig `yaml:"string_value_mapping"`
	MQTTValueScale     float64                   `yaml:"mqtt_value_scale"`
	ErrorValue         *float64                  `yaml:"error_value"`
}

type BlockConfig struct {
	SharedValues MetricConfig   `yaml:"shared"`
	Metrics      []MetricConfig `yaml:"metrics"`
}

// StringValueMappingConfig defines the mapping from string to float
type StringValueMappingConfig struct {
	// ErrorValue was used when no mapping is found in Map
	// deprecated, a warning will be issued to migrate to metric level
	ErrorValue *float64           `yaml:"error_value"`
	Map        map[string]float64 `yaml:"map"`
}

func (mc *MetricConfig) PrometheusDescription() *prometheus.Desc {
	labels := append([]string{"sensor", "topic"}, mc.DynamicLabelsKeys()...)
	return prometheus.NewDesc(
		mc.PrometheusName, mc.Help, labels, mc.ConstantLabels,
	)
}

func (mc *MetricConfig) PrometheusValueType() prometheus.ValueType {
	switch mc.ValueType {
	case GaugeValueType:
		return prometheus.GaugeValue
	case CounterValueType:
		return prometheus.CounterValue
	default:
		return prometheus.UntypedValue
	}
}

func (mc *MetricConfig) DynamicLabelsKeys() []string {
	var labels []string
	for k := range mc.DynamicLabels {
		labels = append(labels, k)
	}
	sort.Strings(labels)
	return labels
}

func LoadConfig(configFile string, logger *zap.Logger) (Config, error) {
	configData, err := ioutil.ReadFile(configFile)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err = yaml.UnmarshalStrict(configData, &cfg); err != nil {
		return cfg, err
	}

	if cfg.MQTT == nil {
		cfg.MQTT = &MQTTConfigDefaults
	}
	if cfg.Cache == nil {
		cfg.Cache = &CacheConfigDefaults
	}
	if cfg.Cache.StateDir == "" {
		cfg.Cache.StateDir = CacheConfigDefaults.StateDir
	}
	if cfg.JsonParsing == nil {
		cfg.JsonParsing = &JsonParsingConfigDefaults
	}
	if cfg.MQTT.DeviceIDRegex == nil {
		cfg.MQTT.DeviceIDRegex = MQTTConfigDefaults.DeviceIDRegex
	}
	var validRegex bool
	for _, name := range cfg.MQTT.DeviceIDRegex.RegEx().SubexpNames() {
		if name == DeviceIDRegexGroup {
			validRegex = true
		}
	}
	if !validRegex {
		return Config{}, fmt.Errorf("device id regex %q does not contain required regex group %q", cfg.MQTT.DeviceIDRegex.pattern, DeviceIDRegexGroup)
	}

	if cfg.MQTT.ObjectPerTopicConfig == nil && cfg.MQTT.MetricPerTopicConfig == nil {
		cfg.MQTT.ObjectPerTopicConfig = &ObjectPerTopicConfig{
			Encoding: EncodingJSON,
		}
	}

	if cfg.MQTT.MetricPerTopicConfig != nil {
		validRegex = false
		for _, name := range cfg.MQTT.MetricPerTopicConfig.MetricNameRegex.RegEx().SubexpNames() {
			if name == MetricNameRegexGroup {
				validRegex = true
			}
		}
		if !validRegex {
			return Config{}, fmt.Errorf("metric name regex %q does not contain required regex group %q", cfg.MQTT.DeviceIDRegex.pattern, MetricNameRegexGroup)
		}
	}

	for _, metric := range cfg.Metrics {
		targets := metric.Metrics
		sources := []MetricConfig{metric.SharedValues, MetricConfigDefaults}
		for _, source := range sources {
			for i := range targets {
				tgt := reflect.ValueOf(&targets[i]).Elem()
				src := reflect.ValueOf(&source).Elem()
				for i := 0; i < src.NumField(); i++ {
					dstField := tgt.FieldByName(src.Type().Field(i).Name)
					if dstField.IsValid() && dstField.CanSet() && dstField.IsZero() &&
						dstField.Type() == src.Field(i).Type() && !src.Field(i).IsZero() {
						dstField.Set(src.Field(i))
					}
				}
			}
		}
	}

	// If any metric forces monotonicy, we need a state directory.
	forcesMonotonicy := false
	for _, blocks := range cfg.Metrics {
		for i, m := range blocks.Metrics {
			if m.ForceMonotonicy {
				forcesMonotonicy = true
			}

			if m.StringValueMapping != nil && m.StringValueMapping.ErrorValue != nil {
				if m.ErrorValue != nil {
					return Config{}, fmt.Errorf("metric %s/%s: cannot set both string_value_mapping.error_value and error_value (string_value_mapping.error_value is deprecated).", m.MQTTName, m.PrometheusName)
				}
				logger.Warn("string_value_mapping.error_value is deprecated: please use error_value at the metric level.", zap.String("prometheusName", m.PrometheusName), zap.String("MQTTName", m.MQTTName))
			}

			// Default for omitted MQTTName
			if m.MQTTName == "" {
				blocks.Metrics[i].MQTTName = m.PrometheusName
			}

			if m.Expression != "" && m.RawExpression != "" {
				return Config{}, fmt.Errorf("metric %s/%s: expression and raw_expression are mutually exclusive.", m.MQTTName, m.PrometheusName)
			}
		}
	}
	if forcesMonotonicy {
		if err := os.MkdirAll(cfg.Cache.StateDir, 0755); err != nil {
			return Config{}, fmt.Errorf("failed to create directory %q: %w", cfg.Cache.StateDir, err)
		}
	}

	return cfg, nil
}
