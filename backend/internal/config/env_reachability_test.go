package config

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

func collectMapstructureKeys(t reflect.Type, prefix string, out map[string]string) {
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.PkgPath != "" {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("mapstructure"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = strings.ToLower(field.Name)
		}
		key := name
		if prefix != "" {
			key = prefix + "." + name
		}

		fieldType := field.Type
		for fieldType.Kind() == reflect.Ptr {
			fieldType = fieldType.Elem()
		}
		if fieldType.Kind() == reflect.Struct {
			collectMapstructureKeys(fieldType, key, out)
			continue
		}
		if fieldType.Kind() == reflect.Map {
			continue
		}
		out[strings.ToLower(key)] = fieldType.String()
	}
}

func TestConfigKeysAreEnvReachable(t *testing.T) {
	bound := map[string]string{}
	collectMapstructureKeys(reflect.TypeOf(Config{}), "", bound)

	viper.Reset()
	t.Cleanup(viper.Reset)
	setDefaults()
	registered := map[string]struct{}{}
	for _, key := range viper.AllKeys() {
		registered[key] = struct{}{}
	}

	var unreachable []string
	for key, kind := range bound {
		if _, ok := registered[key]; !ok {
			unreachable = append(unreachable, key+" ("+kind+")")
		}
	}
	sort.Strings(unreachable)
	if len(unreachable) > 0 {
		t.Fatalf("%d config keys are unreachable from environment variables:\n  %s", len(unreachable), strings.Join(unreachable, "\n  "))
	}
}
