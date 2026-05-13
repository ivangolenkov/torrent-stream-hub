package engine

import (
	"reflect"
	"testing"
)

func TestComputeStreamPieceRangesForBounds(t *testing.T) {
	const mb = int64(1 << 20)
	bounds := streamFileBounds{
		Offset:      0,
		Length:      100 * mb,
		BeginPiece:  0,
		EndPiece:    100,
		DisplayPath: "movie.mkv",
	}

	tests := []struct {
		name   string
		bounds streamFileBounds
		opts   StreamOptions
		want   []streamPieceRange
	}{
		{
			name:   "range in middle adds head and sliding window",
			bounds: bounds,
			opts:   StreamOptions{RangeStart: 50 * mb, HasRange: true},
			want:   []streamPieceRange{{Begin: 0, End: 20}, {Begin: 48, End: 60}},
		},
		{
			name: "mp4 adds tail metadata window",
			bounds: streamFileBounds{
				Offset:      0,
				Length:      100 * mb,
				BeginPiece:  0,
				EndPiece:    100,
				DisplayPath: "MOVIE.MP4",
			},
			opts: StreamOptions{RangeStart: 50 * mb, HasRange: true},
			want: []streamPieceRange{{Begin: 0, End: 20}, {Begin: 48, End: 60}, {Begin: 96, End: 100}},
		},
		{
			name: "small mp4 merges head sliding and tail",
			bounds: streamFileBounds{
				Offset:      0,
				Length:      8 * mb,
				BeginPiece:  0,
				EndPiece:    8,
				DisplayPath: "clip.mov",
			},
			opts: StreamOptions{RangeStart: 0, HasRange: true},
			want: []streamPieceRange{{Begin: 0, End: 8}},
		},
		{
			name:   "range beyond eof keeps only bootstrap windows",
			bounds: bounds,
			opts:   StreamOptions{RangeStart: 150 * mb, HasRange: true},
			want:   []streamPieceRange{{Begin: 0, End: 20}},
		},
		{
			name:   "explicit window bytes limits head and sliding",
			bounds: bounds,
			opts:   StreamOptions{RangeStart: 50 * mb, HasRange: true, WindowBytes: 5 * mb},
			want:   []streamPieceRange{{Begin: 0, End: 5}, {Begin: 48, End: 55}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeStreamPieceRangesForBounds(tt.bounds, mb, 10*mb, tt.opts)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("unexpected ranges: got %+v want %+v", got, tt.want)
			}
		})
	}
}

func TestStreamByteRegionPiecesHandlesUnalignedFile(t *testing.T) {
	bounds := streamFileBounds{
		Offset:      512,
		Length:      2048,
		BeginPiece:  0,
		EndPiece:    3,
		DisplayPath: "video.mkv",
	}

	got, ok := streamByteRegionPieces(bounds, 1024, 1000, 1)
	if !ok {
		t.Fatal("expected range")
	}
	want := streamPieceRange{Begin: 1, End: 2}
	if got != want {
		t.Fatalf("unexpected range: got %+v want %+v", got, want)
	}
}

func TestMergeStreamPieceRanges(t *testing.T) {
	got := mergeStreamPieceRanges([]streamPieceRange{
		{Begin: 10, End: 12},
		{Begin: 1, End: 3},
		{Begin: 2, End: 5},
		{Begin: 12, End: 15},
		{Begin: 20, End: 20},
	})
	want := []streamPieceRange{{Begin: 1, End: 5}, {Begin: 10, End: 15}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected ranges: got %+v want %+v", got, want)
	}
}

func TestAggregateTorrentRangesProtectsOtherFileOverlays(t *testing.T) {
	sm := &StreamManager{states: map[FileKey]*StreamState{
		{Hash: "hash", Index: 1}: {
			AppliedRanges: []streamPieceRange{{Begin: 10, End: 20}},
		},
		{Hash: "other", Index: 0}: {
			AppliedRanges: []streamPieceRange{{Begin: 100, End: 110}},
		},
	}}

	got := sm.aggregateTorrentRanges("hash", FileKey{Hash: "hash", Index: 0}, []streamPieceRange{{Begin: 0, End: 5}, {Begin: 4, End: 12}})
	want := []streamPieceRange{{Begin: 0, End: 20}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected ranges: got %+v want %+v", got, want)
	}
}
