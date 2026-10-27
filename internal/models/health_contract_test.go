package models

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// These DTOs are hand-maintained on both sides of the wire. Check fields, types
// and optionality so a removed operation cannot remain advertised by the GUI.
func TestFrontendHealthContract(t *testing.T) {
	source, err := os.ReadFile("../../web/src/types/index.ts")
	if err != nil {
		t.Fatal(err)
	}
	for _, typ := range []reflect.Type{reflect.TypeOf(BTHealth{}), reflect.TypeOf(BTTorrentHealth{}), reflect.TypeOf(StorageIOTimings{})} {
		body := regexp.MustCompile(`(?s)export interface ` + typ.Name() + ` \{(.*?)\n}`).FindSubmatch(source)
		if body == nil {
			t.Fatal("missing interface", typ.Name())
		}
		actual := map[string]string{}
		for _, field := range regexp.MustCompile(`(\w+)(\??): ([\w\[\]]+);`).FindAllStringSubmatch(string(body[1]), -1) {
			actual[field[1]+field[2]] = field[3]
		}
		expected := map[string]string{}
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			tag := strings.Split(field.Tag.Get("json"), ",")
			name := tag[0]
			if len(tag) > 1 && tag[1] == "omitempty" {
				name += "?"
			}
			var ts string
			switch field.Type.Kind() {
			case reflect.Bool:
				ts = "boolean"
			case reflect.String:
				ts = "string"
				if field.Type.Name() == "TorrentState" {
					ts = "TorrentState"
				}
			case reflect.Slice:
				ts = field.Type.Elem().Name() + "[]"
			case reflect.Pointer:
				if field.Type.Elem().Kind() != reflect.Struct {
					t.Fatalf("unsupported pointer field %s", field.Name)
				}
				ts = field.Type.Elem().Name()
			default:
				ts = "number"
			}
			expected[name] = ts
		}
		if !reflect.DeepEqual(actual, expected) {
			t.Errorf("%s contract differs: Go=%v TS=%v", typ.Name(), expected, actual)
		}
	}
}
