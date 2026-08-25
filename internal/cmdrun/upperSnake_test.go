package cmdrun

import "testing"

func TestUpperSnake(t *testing.T) {
	cases := map[string]string{
		"Case":      "CASE",
		"CaseSHA":   "CASE_SHA",
		"RunID":     "RUN_ID",
		"LocalPath": "LOCAL_PATH",
		"Key":       "KEY",
		"CaseDir":   "CASE_DIR",
		"Trial":     "TRIAL",
		"HTTPServer": "HTTP_SERVER",
		"SHA256":    "SHA256",
	}
	for in, want := range cases {
		if got := upperSnake(in); got != want {
			t.Errorf("upperSnake(%q) = %q, want %q", in, got, want)
		}
	}
}
