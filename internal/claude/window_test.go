package claude

import "testing"

func TestActiveIsDesktop(t *testing.T) {
	for name, tc := range map[string]struct {
		out     string
		want    bool
		wantErr bool
	}{
		"the desktop has focus": {`{"class":"com.anthropic.Claude","title":"Claude"}`, true, false},
		"another window":        {`{"class":"google-chrome","title":"Inbox"}`, false, false},
		"no window has focus":   {`{}`, false, false},
		"not JSON":              {`Invalid`, false, true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := activeIsDesktop([]byte(tc.out))
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Errorf("activeIsDesktop(%s) = %v, %v; want %v, error %v", tc.out, got, err, tc.want, tc.wantErr)
			}
		})
	}
}
