package prometheus

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestDecodeEnvelopeStream(t *testing.T) {
	for _, c := range []struct {
		name, in string
		wantErr  bool
		status   string
		warnings []string
		names    []string
	}{
		{name: "success", in: `{"status":"success","data":[{"__name__":"a"},{"__name__":"b"}]}`,
			status: "success", names: []string{"a", "b"}},
		{name: "fields in any order, unknown skipped", in: `{"data":[{"__name__":"a"}],"infos":["x"],"warnings":["w"],"status":"success"}`,
			status: "success", warnings: []string{"w"}, names: []string{"a"}},
		{name: "empty data", in: `{"status":"success","data":[]}`, status: "success"},
		{name: "error envelope has no data", in: `{"status":"error","errorType":"bad_data","error":"x"}`, status: "error"},
		{name: "data null", in: `{"status":"success","data":null}`, wantErr: true},
		{name: "data object", in: `{"status":"success","data":{"a":1}}`, wantErr: true},
		{name: "not an object", in: `[1,2]`, wantErr: true},
		{name: "truncated", in: `{"status":"success","data":[{"__name__":"a"}`, wantErr: true},
		{name: "element of the wrong type", in: `{"status":"success","data":[1]}`, wantErr: true},
		{name: "status not a string", in: `{"status":1,"data":[]}`, wantErr: true},
		{name: "empty body", in: ``, wantErr: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var status string
			var warnings, names []string
			err := decodeEnvelopeStream(json.NewDecoder(strings.NewReader(c.in)), &status, &warnings, func(dec *json.Decoder) error {
				var s map[string]string
				if err := dec.Decode(&s); err != nil {
					return err
				}
				names = append(names, s["__name__"])
				return nil
			})
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, want error %v", err, c.wantErr)
			}
			if c.wantErr {
				return
			}
			if status != c.status || !reflect.DeepEqual(warnings, c.warnings) || !reflect.DeepEqual(names, c.names) {
				t.Errorf("status %q warnings %v names %v", status, warnings, names)
			}
		})
	}
}
