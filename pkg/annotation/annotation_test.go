package annotation

import (
	"testing"
)

func TestDecode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "containers by name", in: `{"app":{"request":"10Gi","limit":"20Gi"},"init":{"limit":"1Gi"}}`},
		{name: "an empty entry", in: `{"app":{}}`},
		{name: "an empty object", in: `{}`},
		{name: "an unknown field", in: `{"app":{"limits":"1Gi"}}`, wantErr: true},
		{name: "not a quantity", in: `{"app":{"limit":"lots"}}`, wantErr: true},
		{name: "null", in: `null`, wantErr: true},
		{name: "an array", in: `[]`, wantErr: true},
		{name: "data after the object", in: `{} {}`, wantErr: true},
		{name: "empty", in: ``, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Decode(%q) error = %v, want error %v", tc.in, err, tc.wantErr)
			}
		})
	}
}

func TestResources(t *testing.T) {
	v, err := Decode(`{"app":{"request":"1Ki","limit":"1.5"},"zero":{"request":"0","limit":"-1Gi"},"empty":{}}`)
	if err != nil {
		t.Fatal(err)
	}

	res, ok := v.Resources("app")
	if !ok {
		t.Fatal("app is listed")
	}
	if res.Request == nil || *res.Request != 1024 {
		t.Errorf("request = %v, want 1024", res.Request)
	}
	// A fractional byte count rounds up.
	if res.Limit == nil || *res.Limit != 2 {
		t.Errorf("limit = %v, want 2", res.Limit)
	}

	for _, name := range []string{"zero", "empty"} {
		res, ok := v.Resources(name)
		if !ok {
			t.Fatalf("%s is listed", name)
		}
		if res.Request != nil || res.Limit != nil {
			t.Errorf("%s declares %v and %v, want nothing", name, res.Request, res.Limit)
		}
	}

	if _, ok := v.Resources("sidecar"); ok {
		t.Error("sidecar is not listed")
	}
}
