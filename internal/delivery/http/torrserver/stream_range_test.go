package torrserver

import "testing"

func TestParseStreamRange(t *testing.T) {
	tests := []struct {
		name   string
		header string
		size   int64
		want   parsedStreamRange
	}{
		{
			name:   "empty",
			header: "",
			size:   1000,
			want:   parsedStreamRange{Start: 0, End: 999, Valid: true},
		},
		{
			name:   "open ended from zero",
			header: "bytes=0-",
			size:   1000,
			want:   parsedStreamRange{Start: 0, End: 999, HasRange: true, Valid: true},
		},
		{
			name:   "open ended from offset",
			header: "bytes=100-",
			size:   1000,
			want:   parsedStreamRange{Start: 100, End: 999, HasRange: true, Valid: true},
		},
		{
			name:   "closed range",
			header: "bytes=100-199",
			size:   1000,
			want:   parsedStreamRange{Start: 100, End: 199, HasRange: true, Valid: true},
		},
		{
			name:   "suffix range",
			header: "bytes=-500",
			size:   1000,
			want:   parsedStreamRange{Start: 500, End: 999, HasRange: true, Valid: true},
		},
		{
			name:   "suffix larger than file",
			header: "bytes=-1500",
			size:   1000,
			want:   parsedStreamRange{Start: 0, End: 999, HasRange: true, Valid: true},
		},
		{
			name:   "single byte probe",
			header: "bytes=0-0",
			size:   1000,
			want:   parsedStreamRange{Start: 0, End: 0, HasRange: true, Valid: true},
		},
		{
			name:   "start beyond eof",
			header: "bytes=1000-",
			size:   1000,
			want:   parsedStreamRange{Start: 1000, End: 999, HasRange: true, Valid: false},
		},
		{
			name:   "start greater than end",
			header: "bytes=200-100",
			size:   1000,
			want:   parsedStreamRange{HasRange: true, Valid: false},
		},
		{
			name:   "invalid unit",
			header: "items=0-10",
			size:   1000,
			want:   parsedStreamRange{Valid: false},
		},
		{
			name:   "invalid numbers",
			header: "bytes=a-b",
			size:   1000,
			want:   parsedStreamRange{HasRange: true, Valid: false},
		},
		{
			name:   "multi range",
			header: "bytes=0-10,20-30",
			size:   1000,
			want:   parsedStreamRange{HasRange: true, Valid: false, Multi: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseStreamRange(tt.header, tt.size)
			if got != tt.want {
				t.Fatalf("unexpected range: got %+v want %+v", got, tt.want)
			}
		})
	}
}
