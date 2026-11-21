package requestfields

import "testing"

func TestGatewayFieldPolicy(t *testing.T) {
	tests := []struct {
		name, body, mediaType string
		conflict              bool
	}{
		{"ordinary JSON", `{"model":"text","reasoning":{"effort":"low"}}`, "", false},
		{"nested duplicates", `{"reasoning":{"effort":"low","effort":"low"}}`, "", true},
		{"exact continuation duplicate", `{"previous_response_id":"a","previous_response_id":"b"}`, "", true},
		{"same freeform field", `{"tools":[{"parameters":{"properties":{"id":{},"id":{}}}}]}`, "", true},
		{"case-sensitive freeform fields", `{"tools":[{"parameters":{"properties":{"id":{},"ID":{}}}}]}`, "", false},
		{"repeated multipart field", "--boundary\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\ntext\r\n--boundary\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\ntext\r\n--boundary--\r\n", "multipart/form-data; boundary=boundary", true},
		{"multiple uploads", "--boundary\r\nContent-Disposition: form-data; name=\"image[]\"; filename=\"a\"\r\n\r\nA\r\n--boundary\r\nContent-Disposition: form-data; name=\"image[]\"; filename=\"b\"\r\n\r\nB\r\n--boundary--\r\n", "multipart/form-data; boundary=boundary", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasConflict([]byte(tt.body), tt.mediaType); got != tt.conflict {
				t.Fatalf("HasConflict() = %v, want %v", got, tt.conflict)
			}
		})
	}
}
