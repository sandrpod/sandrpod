package aliyun

import "testing"

func TestInstanceIDsJSON_EscapesInjection(t *testing.T) {
	if got := instanceIDsJSON("i-abc"); got != `["i-abc"]` {
		t.Errorf("normal id: got %s", got)
	}
	// An id trying to widen the array must be escaped, not interpolated raw.
	got := instanceIDsJSON(`i-x","i-victim`)
	if got == `["i-x","i-victim"]` {
		t.Errorf("injection not escaped: %s", got)
	}
	if got != `["i-x\",\"i-victim"]` {
		t.Errorf("unexpected escaping: %s", got)
	}
}
